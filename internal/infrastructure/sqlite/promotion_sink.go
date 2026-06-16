package sqlite

import (
	"context"
	"database/sql"

	"github.com/whiskeyjimbo/veska/internal/platform/tokenize"
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

// ftsSink synchronizes the full-text search index tables with the promoted nodes
// within the same transaction to guarantee atomicity.
type ftsSink struct {
	delWords    *sql.Stmt
	delTrigrams *sql.Stmt
	insWords    *sql.Stmt
	insTrigrams *sql.Stmt
}

// NewFTSSink constructs the full-text-search PromotionSink.
func NewFTSSink() PromotionSink { return &ftsSink{} }

func (s *ftsSink) Prepare(ctx context.Context, tx *sql.Tx) error {
	var err error
	// Per-node deletions act as an idempotency safeguard because FTS5 virtual
	// tables would otherwise duplicate rows during multiple promotion attempts
	// within the same transaction.
	if s.delWords, err = tx.PrepareContext(ctx,
		`DELETE FROM node_fts_words WHERE node_id = ? AND branch = ? AND repo_id = ?`); err != nil {
		return err
	}
	if s.delTrigrams, err = tx.PrepareContext(ctx,
		`DELETE FROM node_fts_trigrams WHERE node_id = ? AND branch = ? AND repo_id = ?`); err != nil {
		return err
	}
	if s.insWords, err = tx.PrepareContext(ctx,
		`INSERT INTO node_fts_words (node_id, branch, repo_id, words) VALUES (?, ?, ?, ?)`); err != nil {
		return err
	}
	if s.insTrigrams, err = tx.PrepareContext(ctx,
		`INSERT INTO node_fts_trigrams (node_id, branch, repo_id, raw) VALUES (?, ?, ?, ?)`); err != nil {
		return err
	}
	return nil
}

// BeforeNodeDelete clears full-text search indexes for nodes in the target file
// before the nodes table is updated. Sinks must query the nodes table while
// the old rows still exist so that orphaned FTS records (e.g. from deleted or
// renamed symbols) are successfully cleaned up.
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

// AfterNodeInsert registers full-text indexing words for the newly inserted node.
// File paths are omitted to keep search matches focused on symbol signatures.
func (s *ftsSink) AfterNodeInsert(ctx context.Context, _ *sql.Tx, n nodeWrite, _ int64) error {
	rawFTS := n.Kind + " " + n.Symbol
	wordsFTS := tokenize.Symbol(rawFTS)
	if _, err := s.delWords.ExecContext(ctx, n.NodeID, n.Branch, n.RepoID); err != nil {
		return err
	}
	if _, err := s.delTrigrams.ExecContext(ctx, n.NodeID, n.Branch, n.RepoID); err != nil {
		return err
	}
	if _, err := s.insWords.ExecContext(ctx, n.NodeID, n.Branch, n.RepoID, wordsFTS); err != nil {
		return err
	}
	if _, err := s.insTrigrams.ExecContext(ctx, n.NodeID, n.Branch, n.RepoID, rawFTS); err != nil {
		return err
	}
	return nil
}
