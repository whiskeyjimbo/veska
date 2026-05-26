package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// FindingRepo is the SQLite adapter for the FindingStorage port. It writes
// to the `findings` table created by migration 0003.
//
// The (finding_id, branch) primary key is branch-stable: re-running a check
// against the same anchor on the same branch must not create a duplicate row.
// Save therefore uses ON CONFLICT DO UPDATE so re-detection of an unresolved
// finding refreshes its message/severity without churning the row.
type FindingRepo struct {
	db *sql.DB
}

// NewFindingRepo constructs a FindingRepo bound to the provided write-capable
// *sql.DB handle. The handle must point at a DB that has had migration 0003
// applied (verified by Open()).
func NewFindingRepo(db *sql.DB) *FindingRepo {
	return &FindingRepo{db: db}
}

// Save persists f into the findings table. It is idempotent on (finding_id,
// branch): a second Save with the same finding_id/branch updates rule, message,
// severity, source_layer, state, anchor, and closed_* columns.
//
// Schema fields that are NOT NULL but not present on the domain.Finding value
// are filled in by the adapter:
//   - created_at is set to the current wall-clock millis on first insert.
//   - actor_id / actor_kind default to "service:veska" / "system" when the
//     finding has no actor metadata (the common case for checks produced
//     automatically by the promotion pipeline).
func (r *FindingRepo) Save(ctx context.Context, f *domain.Finding) error {
	if f == nil {
		return fmt.Errorf("sqlite.FindingRepo.Save: nil finding")
	}

	actorID := "service:veska"
	if f.ActorID != nil {
		actorID = *f.ActorID
	}
	actorKind := string(domain.ActorKindSystem)
	if f.ActorKind != nil {
		actorKind = string(*f.ActorKind)
	}

	var (
		nodeIDArg   any
		filePathArg any
	)
	if f.NodeID != nil {
		nodeIDArg = *f.NodeID
	}
	if f.FilePath != nil {
		filePathArg = *f.FilePath
	}

	var closedAtArg any
	if f.ClosedAt != nil {
		closedAtArg = f.ClosedAt.UnixMilli()
	}
	var closedReasonArg any
	if f.ClosedReason != nil {
		closedReasonArg = *f.ClosedReason
	}

	// anchor_content_hash: nil pointer -> NULL; non-nil -> the captured hash.
	// Using sql.NullString keeps the NULL-vs-empty-string distinction explicit
	// at the driver boundary so the revalidation sweep can rely on "NULL means
	// no hash recorded" without coexisting empty strings poisoning the rule.
	var anchorHashArg sql.NullString
	if f.AnchorContentHash != nil {
		anchorHashArg = sql.NullString{String: *f.AnchorContentHash, Valid: true}
	}

	now := time.Now().UnixMilli()

	const stmt = `
INSERT INTO findings (
    finding_id, branch, repo_id, node_id, file_path,
    severity, source_layer, rule, message, state,
    closed_reason, created_at, closed_at, actor_id, actor_kind,
    anchor_content_hash
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(finding_id, branch) DO UPDATE SET
    repo_id             = excluded.repo_id,
    node_id             = excluded.node_id,
    file_path           = excluded.file_path,
    severity            = excluded.severity,
    source_layer        = excluded.source_layer,
    rule                = excluded.rule,
    message             = excluded.message,
    state               = excluded.state,
    closed_reason       = excluded.closed_reason,
    closed_at           = excluded.closed_at,
    actor_id            = excluded.actor_id,
    actor_kind          = excluded.actor_kind,
    anchor_content_hash = excluded.anchor_content_hash`

	_, err := r.db.ExecContext(ctx, stmt,
		f.FindingID, f.Branch, f.RepoID, nodeIDArg, filePathArg,
		string(f.Severity), string(f.SourceLayer), f.Rule, f.Message, string(f.State),
		closedReasonArg, now, closedAtArg, actorID, actorKind,
		anchorHashArg,
	)
	if err != nil {
		return fmt.Errorf("sqlite.FindingRepo.Save: %w", err)
	}
	return nil
}

// CloseObsolete closes the OPEN finding identified by (findingID, branch),
// setting state='closed', closed_reason='revalidated_obsolete', and stamping
// closed_at with the current wall-clock millis (consistent with how Save
// records timestamps).
//
// The UPDATE is gated on state='open' so re-running it cannot churn a finding
// that a human or an earlier pass already closed. A no-op UPDATE (zero rows
// matched — already closed, or no such finding) returns nil; closing a
// finding that does not exist is not an error.
func (r *FindingRepo) CloseObsolete(ctx context.Context, findingID, branch string) error {
	const stmt = `
UPDATE findings
SET state         = 'closed',
    closed_reason = 'revalidated_obsolete',
    closed_at     = ?
WHERE finding_id = ?
  AND branch     = ?
  AND state      = 'open'`

	now := time.Now().UnixMilli()
	if _, err := r.db.ExecContext(ctx, stmt, now, findingID, branch); err != nil {
		return fmt.Errorf("sqlite.FindingRepo.CloseObsolete: %w", err)
	}
	return nil
}

// CloseSupersededAutoLinks closes every OPEN finding with rule='auto-link' in
// (repoID, branch) whose anchor (findings.node_id) is an edge_id of a
// SIMILAR_TO edge whose src_node_id appears in sourceNodeIDs.
//
// See ports.FindingStorage for the full contract. The implementation issues a
// single UPDATE whose WHERE filters by an inner SELECT over the edges table,
// so the supersession is one round-trip irrespective of |sourceNodeIDs|.
//
// SQLite's compile-time SQLITE_MAX_VARIABLE_NUMBER caps the IN-list at ~999;
// to stay safely below that we chunk sourceNodeIDs into batches of 500. An
// empty input is a no-op (returns nil without touching the DB).
func (r *FindingRepo) CloseSupersededAutoLinks(ctx context.Context, repoID, branch string, sourceNodeIDs []string) error {
	if len(sourceNodeIDs) == 0 {
		return nil
	}

	const chunk = 500
	now := time.Now().UnixMilli()
	for start := 0; start < len(sourceNodeIDs); start += chunk {
		end := min(start+chunk, len(sourceNodeIDs))
		batch := sourceNodeIDs[start:end]

		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]

		stmt := `
UPDATE findings
SET state         = 'closed',
    closed_reason = 'revalidated_obsolete',
    closed_at     = ?
WHERE repo_id    = ?
  AND branch     = ?
  AND rule       = 'auto-link'
  AND state      = 'open'
  AND node_id IN (
      SELECT edge_id FROM edges
      WHERE repo_id = ?
        AND branch  = ?
        AND kind    = 'SIMILAR_TO'
        AND src_node_id IN (` + placeholders + `)
  )`

		args := make([]any, 0, 5+len(batch))
		args = append(args, now, repoID, branch, repoID, branch)
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := r.db.ExecContext(ctx, stmt, args...); err != nil {
			return fmt.Errorf("sqlite.FindingRepo.CloseSupersededAutoLinks: %w", err)
		}
	}
	return nil
}
