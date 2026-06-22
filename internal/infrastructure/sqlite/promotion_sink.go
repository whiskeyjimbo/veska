// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
)

// nodeWrite represents the per-node context received by a PromotionSink.
type nodeWrite struct {
	NodeID string
	Branch string
	RepoID string
	Kind   string
	Symbol string
}

// PromotionSink defines transactional hooks that run during node promotion. All
// sinks share the parent promotion transaction; any hook failure aborts the
// entire write batch. BeforeNodeDelete is invoked before the node rows are
// deleted so sinks can query existing nodes, while AfterNodeInsert is run after
// new node records are written.
type PromotionSink interface {
	Prepare(ctx context.Context, tx *sql.Tx) error
	BeforeNodeDelete(ctx context.Context, tx *sql.Tx, branch, repoID, filePath string) error
	AfterNodeInsert(ctx context.Context, tx *sql.Tx, n nodeWrite, promotedAt int64) error
}

// embedRefSink records promoted nodes in node_embedding_refs with a 'pending' state.
// Re-promoting a node resets its embedding status to pending so it is queued for
// re-processing by the background embedder.
type embedRefSink struct {
	upsert *sql.Stmt
}

// NewEmbedRefSink constructs the embedding-ref PromotionSink.
func NewEmbedRefSink() PromotionSink { return &embedRefSink{} }

func (s *embedRefSink) Prepare(ctx context.Context, tx *sql.Tx) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO node_embedding_refs
			(node_id, content_hash, state, enqueued_at, embedded_at)
		VALUES (?, NULL, 'pending', ?, NULL)
		ON CONFLICT(node_id) DO UPDATE SET
			content_hash = NULL,
			state        = 'pending',
			enqueued_at  = excluded.enqueued_at,
			embedded_at  = NULL`)
	if err != nil {
		return err
	}
	s.upsert = stmt
	return nil
}

// BeforeNodeDelete is a no-op because embedding refs are keyed on node IDs.
func (s *embedRefSink) BeforeNodeDelete(context.Context, *sql.Tx, string, string, string) error {
	return nil
}

func (s *embedRefSink) AfterNodeInsert(ctx context.Context, _ *sql.Tx, n nodeWrite, promotedAt int64) error {
	_, err := s.upsert.ExecContext(ctx, n.NodeID, promotedAt)
	return err
}

// ftsSink keeps the full-text-search tables free of orphaned rows when a file
// is re-promoted. It runs synchronously inside the promote tx, but ONLY to
// delete - the rebuild (the expensive FTS5 inserts) is deferred to the async
// WorkKindFTS lane (see internal/application/fts). The delete must stay
// synchronous: it identifies a file's rows by joining to the node table, which
// only works while the OLD node rows are still present (before p.del runs).
type ftsSink struct{}

// NewFTSSink constructs the full-text-search PromotionSink.
func NewFTSSink() PromotionSink { return &ftsSink{} }

// Prepare is a no-op: the sink holds no per-promotion statements now that the
// inserts moved to the async lane.
func (s *ftsSink) Prepare(context.Context, *sql.Tx) error { return nil }

// BeforeNodeDelete clears full-text search indexes for nodes in the target file
// before the nodes table is updated. It must query the nodes table while the
// old rows still exist so that orphaned FTS records (e.g. from deleted or
// renamed symbols) are cleaned up - the async reindex can only see the new
// node set and would otherwise leave a deleted symbol searchable forever.
func (s *ftsSink) BeforeNodeDelete(ctx context.Context, tx *sql.Tx, branch, repoID, filePath string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_fts_words
		 WHERE branch = ? AND repo_id = ?
		   AND node_id IN (SELECT node_id FROM nodes
		                   WHERE file_path = ? AND branch = ? AND repo_id = ?)`,
		branch, repoID, filePath, branch, repoID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_fts_trigrams
		 WHERE branch = ? AND repo_id = ?
		   AND node_id IN (SELECT node_id FROM nodes
		                   WHERE file_path = ? AND branch = ? AND repo_id = ?)`,
		branch, repoID, filePath, branch, repoID,
	); err != nil {
		return err
	}
	return nil
}

// AfterNodeInsert is a no-op: FTS row construction now happens on the async
// WorkKindFTS lane, off the promote critical path.
func (s *ftsSink) AfterNodeInsert(context.Context, *sql.Tx, nodeWrite, int64) error { return nil }
