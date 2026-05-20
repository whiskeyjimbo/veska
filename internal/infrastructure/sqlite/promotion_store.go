package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Compile-time assertion that PromotionStore satisfies the application port.
var _ application.PromotionStore = (*PromotionStore)(nil)

// PromotionStore is the SQLite adapter for the application.PromotionStore port.
// It owns the entire promotion transaction: registration check, BEGIN
// IMMEDIATE serializable tx, per-file node delete + re-insert, the registered
// co-transactional PromotionSinks, the post_promotion_queue inserts, and the
// commit. Any error rolls the whole transaction back.
//
// PromotionSinks (FTS, embedding-refs, and any future co-transactional writer)
// are registered at construction time, so adding a sink does not require
// editing the transaction body — the store is open for extension, closed for
// modification.
type PromotionStore struct {
	writeDB   *sql.DB
	sinks     []PromotionSink
	workKinds []string
}

// PromotionStoreOption configures a PromotionStore at construction time.
type PromotionStoreOption func(*PromotionStore)

// WithReviewEnabled gates the optional WorkKindReview lane. When enabled, the
// store enqueues a per-file 'review' queue row in addition to the always-on
// post-promotion kinds; when disabled (the default) no review row is enqueued.
func WithReviewEnabled(enabled bool) PromotionStoreOption {
	return func(s *PromotionStore) {
		s.workKinds = application.PromotionWorkKinds(enabled)
	}
}

// NewPromotionStore constructs a PromotionStore over the write-capable DB
// handle and the given co-transactional sinks. Sinks run in registration order
// inside the promotion transaction.
func NewPromotionStore(writeDB *sql.DB, sinks []PromotionSink, opts ...PromotionStoreOption) *PromotionStore {
	s := &PromotionStore{
		writeDB:   writeDB,
		sinks:     sinks,
		workKinds: application.PromotionWorkKinds(false),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Promote writes the batch in a single atomic transaction. It returns
// application.ErrUnregisteredRepo when the batch's repo is not registered.
func (s *PromotionStore) Promote(ctx context.Context, batch application.PromotionBatch) error {
	repoID := batch.RepoID
	branch := batch.Branch

	// Reject promotions for repos not in the registry.
	var exists int
	err := s.writeDB.QueryRowContext(ctx,
		`SELECT 1 FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return application.ErrUnregisteredRepo{RepoID: repoID}
	}
	if err != nil {
		return fmt.Errorf("promoter: check repo registration: %w", err)
	}

	// An empty batch confirms registration but opens no transaction — there is
	// nothing to write.
	if len(batch.Files) == 0 {
		return nil
	}

	tx, err := s.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
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
			 line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
			 signature, prev_signature)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare insert: %w", err)
	}
	defer insStmt.Close()

	// Snapshot the prior signature for each (node_id) in (file, branch, repo)
	// BEFORE the per-file DELETE so the new row can carry it forward as
	// prev_signature. This is what powers the contract-drift check without
	// requiring a separate history table.
	prevSigSelectStmt, err := tx.PrepareContext(ctx, `
		SELECT node_id, signature FROM nodes
		WHERE file_path = ? AND branch = ? AND repo_id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare prev-sig select: %w", err)
	}
	defer prevSigSelectStmt.Close()

	queueStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: prepare queue insert: %w", err)
	}
	defer queueStmt.Close()

	// Prepare each co-transactional sink once against the tx.
	for _, sink := range s.sinks {
		if err := sink.Prepare(ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: prepare sink: %w", err)
		}
	}

	now := batch.PromotedAt

	for _, file := range batch.Files {
		filePath := file.Path

		// Capture prior signatures keyed by node_id BEFORE the DELETE clears
		// them, so we can thread prev_signature into the re-inserted rows.
		// Nodes with NULL signature in the prior row map to a nil pointer so
		// the new row's prev_signature remains NULL — equivalent to "no prior
		// signature known" rather than "" which would falsely register as a
		// drift to/from the empty string.
		prevSig := make(map[string]*string)
		prevRows, err := prevSigSelectStmt.QueryContext(ctx, filePath, branch, repoID)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: select prev signatures for %q: %w", filePath, err)
		}
		for prevRows.Next() {
			var nodeID string
			var sig sql.NullString
			if err := prevRows.Scan(&nodeID, &sig); err != nil {
				_ = prevRows.Close()
				_ = tx.Rollback()
				return fmt.Errorf("promoter: scan prev signature for %q: %w", filePath, err)
			}
			if sig.Valid {
				v := sig.String
				prevSig[nodeID] = &v
			} else {
				prevSig[nodeID] = nil
			}
		}
		if err := prevRows.Err(); err != nil {
			_ = prevRows.Close()
			_ = tx.Rollback()
			return fmt.Errorf("promoter: iterate prev signatures for %q: %w", filePath, err)
		}
		if err := prevRows.Close(); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: close prev signatures for %q: %w", filePath, err)
		}

		// Sink pre-delete hooks run while the old node rows still exist — e.g.
		// the FTS sink's node_id IN (SELECT ... FROM nodes ...) deletes MUST
		// resolve against the pre-DELETE rows.
		for _, sink := range s.sinks {
			if err := sink.BeforeNodeDelete(ctx, tx, branch, repoID, filePath); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: sink before-delete for %q: %w", filePath, err)
			}
		}

		// Delete all existing nodes for this file+branch+repo before re-inserting.
		if _, err := delStmt.ExecContext(ctx, filePath, branch, repoID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: delete nodes for %q: %w", filePath, err)
		}

		// Upsert all nodes for this file.
		for _, n := range file.Nodes {
			lang := nodeLanguage(n)
			lineStart, lineEnd := nodeLines(n)
			contentHash := nodeContentHash(n)
			sig := nodeSignature(n)
			// prev signature: NULL when there was no prior row for this node_id
			// in (file, branch) — first-time promotions cannot drift.
			var prev any
			if ps, ok := prevSig[string(n.ID)]; ok && ps != nil {
				prev = *ps
			} else {
				prev = nil
			}

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
				batch.Actor.ID,
				string(batch.Actor.Kind),
				sig,
				prev,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: insert node %q: %w", n.ID, err)
			}

			// Per-node co-transactional sink writes (FTS, embedding-refs).
			nw := nodeWrite{
				NodeID: string(n.ID),
				Branch: branch,
				RepoID: repoID,
				Kind:   string(n.Kind),
				Symbol: n.Name,
			}
			for _, sink := range s.sinks {
				if err := sink.AfterNodeInsert(ctx, tx, nw, now); err != nil {
					_ = tx.Rollback()
					return fmt.Errorf("promoter: sink after-insert for %q: %w", n.ID, err)
				}
			}
		}

		// Enqueue one row per work_kind for this file.
		for _, wk := range s.workKinds {
			if _, err := queueStmt.ExecContext(ctx,
				batch.GitSHA, repoID, branch, batch.GitSHA, wk, filePath, now,
			); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("promoter: enqueue %q for %q: %w", wk, filePath, err)
			}
		}
	}

	// Enqueue exactly one repo-scoped WorkKindWiki row per promotion (not
	// per-file). The wiki lane regenerates the whole hot_zone + entry_points
	// surfaces, so a single row per promotion is sufficient; the payload is
	// empty because the handler operates on repo-scoped state.
	if _, err := queueStmt.ExecContext(ctx,
		batch.GitSHA, repoID, branch, batch.GitSHA, string(ports.WorkKindWiki), "", now,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("promoter: enqueue wiki: %w", err)
	}

	// Advance repos.last_promoted_sha (and repos.active_branch when the
	// caller supplied one) atomically with the node writes. Without this,
	// StartupResync's cheap-path check (LastPromotedSHA == HEAD) has nothing
	// to compare against — every daemon restart treats every repo as
	// never-promoted and re-runs the full reparser (solov2-c47).
	//
	// An empty SHA is treated as caller error and skipped so we don't clobber
	// a known-good value with "". An empty branch is a real production case
	// (repo.Add does not set active_branch), so we write the SHA alone in
	// that case and leave active_branch untouched.
	if batch.GitSHA != "" {
		var execErr error
		if branch != "" {
			_, execErr = tx.ExecContext(ctx,
				`UPDATE repos SET last_promoted_sha = ?, active_branch = ? WHERE repo_id = ?`,
				batch.GitSHA, branch, repoID,
			)
		} else {
			_, execErr = tx.ExecContext(ctx,
				`UPDATE repos SET last_promoted_sha = ? WHERE repo_id = ?`,
				batch.GitSHA, repoID,
			)
		}
		if execErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("promoter: advance last_promoted_sha: %w", execErr)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("promoter: commit: %w", err)
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

// nodeLines returns (lineStart, lineEnd) as values so NULL is written when the
// node has no line range.
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

// nodeSignature returns the signature string for the INSERT bind, or nil so
// SQLite writes a NULL when the parser did not populate it. Returning the
// empty string here would conflate "unknown signature" with "known empty
// signature" and break the contract-drift comparison.
func nodeSignature(n *domain.Node) any {
	if n.Signature == nil {
		return nil
	}
	return *n.Signature
}
