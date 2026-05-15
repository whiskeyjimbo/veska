package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RevalidateRepo is the SQLite adapter for the RevalidateQuerier port. It
// reads the join of findings × nodes scoped by (repo_id, branch, file_path)
// to discover findings whose recorded anchor_content_hash no longer matches
// the current node's content_hash, and writes a state='closed' update with
// closed_reason='revalidated_obsolete'.
//
// The repo is intentionally write-narrow: only the closed_* / state / actor
// columns are touched. The original creator (actor metadata at emission
// time) is preserved on the row's history via closed_by columns owned by
// this UPDATE — the schema today carries no separate audit log table, so
// these columns are the only place revalidation provenance lives.
type RevalidateRepo struct {
	db *sql.DB
}

// NewRevalidateRepo constructs a RevalidateRepo bound to the provided
// write-capable *sql.DB. The handle must point at a DB that has had
// migrations 0003 (findings) and 0008 (anchor_content_hash) applied; both
// are verified by Open().
func NewRevalidateRepo(db *sql.DB) *RevalidateRepo {
	return &RevalidateRepo{db: db}
}

// StaleFindingsForFile returns the set of open findings on (repoID, branch)
// whose anchor node lives in filePath AND whose recorded
// anchor_content_hash differs from the node's current content_hash.
//
// Rows with anchor_content_hash IS NULL (file-anchored findings such as
// parse-failure) are filtered out — there is no hash to compare against.
//
// Rows whose anchor node has no matching row in `nodes` (deleted symbol)
// are filtered out by the INNER JOIN; cleaning those up is a separate
// path (out of scope for this port).
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

// CloseAsRevalidatedObsolete flips the named finding row to:
//
//	state         = 'closed'
//	closed_reason = 'revalidated_obsolete'
//	closed_at     = closedAt
//	actor_id      = 'service:veska'
//	actor_kind    = 'system'
//
// The UPDATE is gated on state='open' so re-delivery of the same revalidate
// queue row cannot churn a finding that was already closed by an earlier
// pass (or by a human). A no-op UPDATE returns nil.
func (r *RevalidateRepo) CloseAsRevalidatedObsolete(
	ctx context.Context, repoID, branch, findingID string, closedAt int64,
) error {
	const stmt = `
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

	if _, err := r.db.ExecContext(ctx, stmt, closedAt, findingID, branch, repoID); err != nil {
		return fmt.Errorf("sqlite.RevalidateRepo.CloseAsRevalidatedObsolete: %w", err)
	}
	return nil
}

// HasInboundEdges reports whether the named node currently has at least one
// inbound edge on (repoID, branch). Uses LIMIT 1 + EXISTS so the query
// short-circuits at the first matching row; the (dst_node_id, branch, kind)
// index on edges keeps this constant-time.
func (r *RevalidateRepo) HasInboundEdges(
	ctx context.Context, repoID, branch, nodeID string,
) (bool, error) {
	const q = `
SELECT EXISTS (
    SELECT 1
    FROM edges
    WHERE dst_node_id = ?
      AND branch      = ?
      AND repo_id     = ?
    LIMIT 1
)`
	var has bool
	if err := r.db.QueryRowContext(ctx, q, nodeID, branch, repoID).Scan(&has); err != nil {
		return false, fmt.Errorf("sqlite.RevalidateRepo.HasInboundEdges: %w", err)
	}
	return has, nil
}

// NodeSignaturePair returns (prev_signature, signature) for the node. If the
// node row is absent, returns ("", "", nil) — the caller treats that as
// "drift resolved" (close the finding). NULL columns also surface as "".
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

// RefreshAnchorHash rewrites findings.anchor_content_hash for the named row
// so a subsequent revalidation sweep does not re-fire on the same drift.
// State stays 'open'; closed_reason stays NULL. The UPDATE is gated on
// state='open' so already-closed rows are not resurrected. `at` is accepted
// for forward-compat (no audit column today) but is not currently written.
func (r *RevalidateRepo) RefreshAnchorHash(
	ctx context.Context, repoID, branch, findingID, newHash string, at int64,
) error {
	_ = at // reserved for future audit-column work; see port doc.
	const stmt = `
UPDATE findings
SET anchor_content_hash = ?
WHERE finding_id = ?
  AND branch     = ?
  AND repo_id    = ?
  AND state      = 'open'`
	if _, err := r.db.ExecContext(ctx, stmt, newHash, findingID, branch, repoID); err != nil {
		return fmt.Errorf("sqlite.RevalidateRepo.RefreshAnchorHash: %w", err)
	}
	return nil
}

// ApplyDecisions applies a batch of refresh + close decisions inside one
// transaction, collapsing fsyncs from O(decisions) to one per call. See the
// port doc for full semantics.
//
// Implementation notes:
//   - Empty input: returns nil without opening a tx.
//   - Both UPDATE statements are prepared once (lazily, only if the batch
//     contains decisions of that kind) and reused for every row of the
//     matching kind.
//   - state='open' gates on both UPDATEs mean idempotent on already-closed
//     rows (matched=0 is fine).
//   - On any error (incl. Commit) the tx rolls back; the caller treats this
//     as a retryable failure and MUST NOT bump success counters.
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
	// Track commit success so the deferred rollback is a true cleanup path
	// (it becomes a no-op once tx.Commit returns nil; calling Rollback
	// after a successful Commit is a sql.ErrTxDone we deliberately ignore).
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

// Compile-time check: *RevalidateRepo satisfies the port.
var _ ports.RevalidateQuerier = (*RevalidateRepo)(nil)
