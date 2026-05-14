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
SELECT f.finding_id, f.node_id, f.anchor_content_hash, n.content_hash
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
		if err := rows.Scan(&s.FindingID, &nodeID, &anchorHash, &s.CurrentHash); err != nil {
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

// Compile-time check: *RevalidateRepo satisfies the port.
var _ ports.RevalidateQuerier = (*RevalidateRepo)(nil)
