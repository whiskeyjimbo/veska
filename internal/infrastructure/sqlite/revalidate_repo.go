// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RevalidateRepo is the SQLite adapter for the RevalidateQuerier port. It queries
// findings whose recorded anchor hashes diverge from current node content hashes,
// and marks them closed. The update preserves creator metadata on the row's
// history since the schema does not include a separate audit log table.
type RevalidateRepo struct {
	db *sql.DB
}

// NewRevalidateRepo constructs a RevalidateRepo using the provided database handle.
func NewRevalidateRepo(db *sql.DB) *RevalidateRepo {
	return &RevalidateRepo{db: db}
}

// StaleFindingsForFile returns open findings whose recorded anchor hashes differ
// from current node content hashes. Findings with NULL anchor hashes are ignored,
// and deleted symbols are filtered out by the inner join.
func (r *RevalidateRepo) StaleFindingsForFile(
	ctx context.Context, repoID, branch, filePath string,
) ([]ports.StaleFinding, error) {
	const q = `
SELECT f.finding_id, f.node_id, f.rule, f.anchor_content_hash, n.content_hash
FROM findings AS f
JOIN nodes   AS n
  ON  n.node_id  = f.node_id
  AND n.branch   = f.branch
  AND n.repo_id  = f.repo_id
WHERE f.repo_id              = ?
  AND f.branch               = ?
  AND f.state                = 'open'
  AND f.anchor_content_hash IS NOT NULL
  AND n.file_path            = ?
  AND n.content_hash        != f.anchor_content_hash`

	rows, err := r.db.QueryContext(ctx, q, repoID, branch, filePath)
	if err != nil {
		return nil, fmt.Errorf("sqlite.RevalidateRepo.StaleFindingsForFile: query: %w", err)
	}
	defer rows.Close()

	var out []ports.StaleFinding
	for rows.Next() {
		var (
			s          ports.StaleFinding
			anchorHash sql.NullString
			nodeID     sql.NullString
		)
		if err := rows.Scan(&s.FindingID, &nodeID, &s.Rule, &anchorHash, &s.CurrentHash); err != nil {
			return nil, fmt.Errorf("sqlite.RevalidateRepo.StaleFindingsForFile: scan: %w", err)
		}
		// The IS NOT NULL filter in the WHERE clause guarantees this, but
		// guard against driver-level surprises and ignore-empty.
		if anchorHash.Valid {
			s.AnchorHash = anchorHash.String
		}
		if nodeID.Valid {
			s.NodeID = nodeID.String
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.RevalidateRepo.StaleFindingsForFile: rows: %w", err)
	}
	return out, nil
}

// HasInboundCallEdges reports whether a node has at least one inbound CALLS edge.
// Non-caller relationships (such as CONTAINS or IMPORTS parent edges) are ignored.
// The query uses an EXISTS clause and short-circuits on the first match.
func (r *RevalidateRepo) HasInboundCallEdges(
	ctx context.Context, repoID, branch, nodeID string,
) (bool, error) {
	const q = `
SELECT EXISTS (
    SELECT 1
    FROM edges
    WHERE dst_node_id = ?
      AND branch      = ?
      AND repo_id     = ?
      AND kind        = 'CALLS'
    LIMIT 1
)`
	var has bool
	if err := r.db.QueryRowContext(ctx, q, nodeID, branch, repoID).Scan(&has); err != nil {
		return false, fmt.Errorf("sqlite.RevalidateRepo.HasInboundCallEdges: %w", err)
	}
	return has, nil
}

// HasTestCaller reports whether a node has an inbound CALLS edge originating
// from a test file. The test file check is executed in Go using pathfilter
// to avoid embedding test file naming patterns directly into the SQL query.
func (r *RevalidateRepo) HasTestCaller(
	ctx context.Context, repoID, branch, nodeID string,
) (bool, error) {
	const q = `
SELECT DISTINCT src.file_path
FROM edges e
JOIN nodes src
  ON  src.node_id = e.src_node_id
  AND src.branch  = e.branch
  AND src.repo_id = e.repo_id
WHERE e.dst_node_id = ?
  AND e.branch      = ?
  AND e.repo_id     = ?
  AND e.kind        = 'CALLS'`
	rows, err := r.db.QueryContext(ctx, q, nodeID, branch, repoID)
	if err != nil {
		return false, fmt.Errorf("sqlite.RevalidateRepo.HasTestCaller: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fp sql.NullString
		if err := rows.Scan(&fp); err != nil {
			return false, fmt.Errorf("sqlite.RevalidateRepo.HasTestCaller: scan: %w", err)
		}
		if fp.Valid && pathfilter.IsTestFile(fp.String) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("sqlite.RevalidateRepo.HasTestCaller: rows: %w", err)
	}
	return false, nil
}

// NodeSignaturePair retrieves the previous and current signatures for a node.
// A missing node returns empty signatures without an error.
func (r *RevalidateRepo) NodeSignaturePair(
	ctx context.Context, repoID, branch, nodeID string,
) (prev, current string, err error) {
	const q = `
SELECT prev_signature, signature
FROM nodes
WHERE node_id = ? AND branch = ? AND repo_id = ?`
	var p, c sql.NullString
	row := r.db.QueryRowContext(ctx, q, nodeID, branch, repoID)
	switch err := row.Scan(&p, &c); {
	case err == sql.ErrNoRows:
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("sqlite.RevalidateRepo.NodeSignaturePair: %w", err)
	}
	if p.Valid {
		prev = p.String
	}
	if c.Valid {
		current = c.String
	}
	return prev, current, nil
}

// ApplyDecisions executes a batch of refresh and close decisions within a
// single transaction. Prepared statements are initialized lazily and reused
// per decision type to minimize database overhead.
func (r *RevalidateRepo) ApplyDecisions(
	ctx context.Context, repoID, branch string, decisions []ports.FindingDecision, at int64,
) error {
	if len(decisions) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: begin: %w", err)
	}
	// Roll back the transaction on deferred cleanup if commit did not succeed.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	const refreshStmt = `
UPDATE findings
SET anchor_content_hash = ?
WHERE finding_id = ?
  AND branch     = ?
  AND repo_id    = ?
  AND state      = 'open'`

	const closeStmt = `
UPDATE findings
SET state         = 'closed',
    closed_reason = 'revalidated_obsolete',
    closed_at     = ?,
    actor_id      = 'service:veska',
    actor_kind    = 'system'
WHERE finding_id = ?
  AND branch     = ?
  AND repo_id    = ?
  AND state      = 'open'`

	var (
		refreshPS *sql.Stmt
		closePS   *sql.Stmt
	)
	defer func() {
		if refreshPS != nil {
			refreshPS.Close()
		}
		if closePS != nil {
			closePS.Close()
		}
	}()

	for i, d := range decisions {
		switch d.Kind {
		case ports.DecisionRefresh:
			if refreshPS == nil {
				ps, perr := tx.PrepareContext(ctx, refreshStmt)
				if perr != nil {
					return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: prepare refresh: %w", perr)
				}
				refreshPS = ps
			}
			if _, exErr := refreshPS.ExecContext(ctx, d.NewHash, d.FindingID, branch, repoID); exErr != nil {
				return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: refresh #%d (%s): %w", i, d.FindingID, exErr)
			}
		case ports.DecisionClose:
			if closePS == nil {
				ps, perr := tx.PrepareContext(ctx, closeStmt)
				if perr != nil {
					return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: prepare close: %w", perr)
				}
				closePS = ps
			}
			if _, exErr := closePS.ExecContext(ctx, at, d.FindingID, branch, repoID); exErr != nil {
				return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: close #%d (%s): %w", i, d.FindingID, exErr)
			}
		default:
			return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: decision #%d (%s): unknown kind %d", i, d.FindingID, d.Kind)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite.RevalidateRepo.ApplyDecisions: commit: %w", err)
	}
	committed = true
	return nil
}

var _ ports.RevalidateQuerier = (*RevalidateRepo)(nil)
