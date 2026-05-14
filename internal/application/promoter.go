package application

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// workKinds lists the post-promotion work kinds enqueued per file.
var workKinds = []string{"embed", "auto_link", "revalidate"}

// ErrUnregisteredRepo is returned by Promote when the repoID is not found in
// the repos table. The daemon must never promote work from an unknown repo.
type ErrUnregisteredRepo struct{ RepoID string }

func (e ErrUnregisteredRepo) Error() string {
	return fmt.Sprintf(
		"promoter: repo %q is not registered — run: veska repo add <path>",
		e.RepoID,
	)
}

// Promoter flushes the in-memory StagingArea to SQLite in a single atomic
// BEGIN IMMEDIATE transaction and enqueues post-promotion work items.
type Promoter struct {
	staging *StagingArea
	writeDB *sql.DB
}

// NewPromoter constructs a Promoter wired to the provided StagingArea and
// write-only SQLite handle.
func NewPromoter(staging *StagingArea, writeDB *sql.DB) *Promoter {
	return &Promoter{
		staging: staging,
		writeDB: writeDB,
	}
}

// Promote is called by the post-commit hook.  It:
//  1. Takes a snapshot of all nodes staged for (repoID, branch).
//  2. Opens a BEGIN IMMEDIATE transaction.
//  3. For each staged file: deletes existing nodes then upserts the new ones.
//  4. Enqueues one post_promotion_queue row per file per work_kind inside the
//     same transaction.
//  5. Commits — all writes land atomically or not at all.
//  6. Calls StagingArea.DeleteStagedFile for each promoted file after commit.
//
// actor records who triggered the promotion. Hook-triggered paths should pass
// domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}.
func (p *Promoter) Promote(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error {
	// Reject promotions for repos not in the registry.
	var exists int
	err := p.writeDB.QueryRowContext(ctx,
		`SELECT 1 FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return ErrUnregisteredRepo{RepoID: repoID}
	}
	if err != nil {
		return fmt.Errorf("promoter: check repo registration: %w", err)
	}

	snap := p.staging.Snapshot(repoID, branch)
	if len(snap) == 0 {
		return nil
	}

	tx, err := p.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("promoter: begin tx: %w", err)
	}

	// Prepare statements within the transaction for efficiency.
	delStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM nodes WHERE file_path = ? AND branch = ? AND repo_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare delete: %w", err)
	}
	defer delStmt.Close()

	insStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO nodes
			(node_id, branch, repo_id, language, kind, symbol_path, file_path,
			 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare insert: %w", err)
	}
	defer insStmt.Close()

	queueStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare queue insert: %w", err)
	}
	defer queueStmt.Close()

	now := time.Now().UnixMilli()

	for filePath, nodes := range snap {
		// Delete all existing nodes for this file+branch+repo before re-inserting.
		if _, err := delStmt.ExecContext(ctx, filePath, branch, repoID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete nodes for %q: %w", filePath, err)
		}

		// Upsert all nodes for this file.
		for _, n := range nodes {
			lang := nodeLanguage(n)
			lineStart, lineEnd := nodeLines(n)
			contentHash := nodeContentHash(n)

			if _, err := insStmt.ExecContext(ctx,
				string(n.ID),
				branch,
				repoID,
				lang,
				string(n.Kind),
				n.Name,
				n.Path,
				lineStart,
				lineEnd,
				contentHash,
				now,
				actor.ID,
				string(actor.Kind),
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert node %q: %w", n.ID, err)
			}
		}

		// Enqueue one row per work_kind for this file.
		for _, wk := range workKinds {
			if _, err := queueStmt.ExecContext(ctx,
				gitSHA, repoID, branch, gitSHA, wk, filePath, now,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: enqueue %q for %q: %w", wk, filePath, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("promoter: commit: %w", err)
	}

	// Clear staging entries only after a successful commit.
	for filePath := range snap {
		p.staging.DeleteStagedFile(repoID, branch, filePath)
	}

	return nil
}

// nodeLanguage returns the language string or "" when not set.
func nodeLanguage(n *domain.Node) string {
	if n.Language == nil {
		return ""
	}
	return *n.Language
}

// nodeLines returns (lineStart, lineEnd) as sql.NullInt64 pairs so NULL is
// written when the node has no line range.
func nodeLines(n *domain.Node) (lineStart, lineEnd any) {
	if n.Lines == nil {
		return nil, nil
	}
	return n.Lines.Start, n.Lines.End
}

// nodeContentHash returns the content hash string or "" when not set.
func nodeContentHash(n *domain.Node) string {
	if n.ContentHash == nil {
		return ""
	}
	return string(*n.ContentHash)
}
