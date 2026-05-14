package sqlite

import (
	"context"
	"database/sql"
	"fmt"
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

	now := time.Now().UnixMilli()

	const stmt = `
INSERT INTO findings (
    finding_id, branch, repo_id, node_id, file_path,
    severity, source_layer, rule, message, state,
    closed_reason, created_at, closed_at, actor_id, actor_kind
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(finding_id, branch) DO UPDATE SET
    repo_id       = excluded.repo_id,
    node_id       = excluded.node_id,
    file_path     = excluded.file_path,
    severity      = excluded.severity,
    source_layer  = excluded.source_layer,
    rule          = excluded.rule,
    message       = excluded.message,
    state         = excluded.state,
    closed_reason = excluded.closed_reason,
    closed_at     = excluded.closed_at,
    actor_id      = excluded.actor_id,
    actor_kind    = excluded.actor_kind`

	_, err := r.db.ExecContext(ctx, stmt,
		f.FindingID, f.Branch, f.RepoID, nodeIDArg, filePathArg,
		string(f.Severity), string(f.SourceLayer), f.Rule, f.Message, string(f.State),
		closedReasonArg, now, closedAtArg, actorID, actorKind,
	)
	if err != nil {
		return fmt.Errorf("sqlite.FindingRepo.Save: %w", err)
	}
	return nil
}
