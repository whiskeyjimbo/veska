package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// FindingRepo implements the FindingStorage port. Because the (finding_id,
// branch) primary key is branch-stable, re-running a check on the same branch
// must not create duplicates. Save therefore uses ON CONFLICT DO UPDATE to
// refresh the finding's properties without churning the row.
type FindingRepo struct {
	db *sql.DB
}

// NewFindingRepo constructs a FindingRepo. The database handle must be
// write-capable and have migration 0003 applied.
func NewFindingRepo(db *sql.DB) *FindingRepo {
	return &FindingRepo{db: db}
}

// Save persists a finding into the database, performing an idempotent update
// if the finding already exists on the branch. Fields not defined on the
// domain object, such as default actor metadata for automated checks, are
// filled in with default values.
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

	// Using sql.NullString maintains the distinction between a NULL and an empty
	// string, allowing the revalidation sweep to reliably distinguish between
	// a missing hash and an empty hash value.
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

// CloseObsolete transitions an open finding's state to closed. The update is
// gated on the open state to prevent modifying findings that have already been
// closed manually or by another process.
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

// CloseSupersededByRule closes open findings under a rule that are not in the
// keep set. The keep set is chunked to stay safely under SQLite's maximum query
// variable limits.
func (r *FindingRepo) CloseSupersededByRule(ctx context.Context, repoID, branch, rule string, keep []string) error {
	now := time.Now().UnixMilli()

	if len(keep) == 0 {
		const stmt = `
UPDATE findings
SET state         = 'closed',
    closed_reason = 'revalidated_obsolete',
    closed_at     = ?
WHERE repo_id = ?
  AND branch  = ?
  AND rule    = ?
  AND state   = 'open'`
		if _, err := r.db.ExecContext(ctx, stmt, now, repoID, branch, rule); err != nil {
			return fmt.Errorf("sqlite.FindingRepo.CloseSupersededByRule: %w", err)
		}
		return nil
	}

	const maxInline = 500
	if len(keep) > maxInline {
		// A set-difference fallback is used for keep sets that exceed the chunk
		// budget because multiple chunked NOT IN queries would incorrectly close
		// findings omitted from any single chunk.
		return r.closeSupersededByRuleSetDiff(ctx, repoID, branch, rule, keep, now)
	}

	placeholders := strings.Repeat("?,", len(keep))
	placeholders = placeholders[:len(placeholders)-1]

	stmt := `
UPDATE findings
SET state         = 'closed',
    closed_reason = 'revalidated_obsolete',
    closed_at     = ?
WHERE repo_id    = ?
  AND branch     = ?
  AND rule       = ?
  AND state      = 'open'
  AND finding_id NOT IN (` + placeholders + `)`

	args := make([]any, 0, 4+len(keep))
	args = append(args, now, repoID, branch, rule)
	for _, id := range keep {
		args = append(args, id)
	}
	if _, err := r.db.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("sqlite.FindingRepo.CloseSupersededByRule: %w", err)
	}
	return nil
}

// closeSupersededByRuleSetDiff computes the set difference in memory to close
// superseded findings when the keep set size exceeds the single-query chunk
// limit.
func (r *FindingRepo) closeSupersededByRuleSetDiff(ctx context.Context, repoID, branch, rule string, keep []string, now int64) error {
	const sel = `SELECT finding_id FROM findings WHERE repo_id=? AND branch=? AND rule=? AND state='open'`
	rows, err := r.db.QueryContext(ctx, sel, repoID, branch, rule)
	if err != nil {
		return fmt.Errorf("sqlite.FindingRepo.CloseSupersededByRule: %w", err)
	}
	defer rows.Close()
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	var toClose []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("sqlite.FindingRepo.CloseSupersededByRule: %w", err)
		}
		if _, k := keepSet[id]; !k {
			toClose = append(toClose, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite.FindingRepo.CloseSupersededByRule: %w", err)
	}
	const upd = `UPDATE findings SET state='closed', closed_reason='revalidated_obsolete', closed_at=? WHERE finding_id=? AND branch=? AND state='open'`
	for _, id := range toClose {
		if _, err := r.db.ExecContext(ctx, upd, now, id, branch); err != nil {
			return fmt.Errorf("sqlite.FindingRepo.CloseSupersededByRule: %w", err)
		}
	}
	return nil
}

// CloseSupersededAutoLinks closes open auto-link findings whose anchors match
// similar-to edges originating from the specified source nodes. The query is
// chunked into batches of 500 to prevent exceeding SQLITE_MAX_VARIABLE_NUMBER.
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
