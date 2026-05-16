package sqlite

import (
	"context"
	"database/sql"

	"github.com/whiskeyjimbo/veska/internal/tokenize"
)

// nodeWrite is the per-node context a PromotionSink receives after a node row
// has been inserted. It carries the identity and parse-derived text a sink
// needs without exposing the SQL bind layout.
type nodeWrite struct {
	NodeID string
	Branch string
	RepoID string
	Kind   string
	Symbol string // domain Node.Name — the qualified symbol path
}

// PromotionSink is a co-transactional writer plugged into PromotionStore. Every
// sink participates in the single promotion transaction; a failure in any hook
// rolls the whole transaction back.
//
// Lifecycle within one Promote call:
//
//	Prepare(tx)                       — once, before the per-file loop
//	for each file:
//	    BeforeNodeDelete(ctx, tx, file)   — before the store DELETEs node rows
//	    (store DELETEs + re-INSERTs node rows)
//	    for each node: AfterNodeInsert(...) — after each node row is inserted
//
// BeforeNodeDelete is the hook for writes that must observe the still-present
// prior node rows (e.g. FTS deletes that resolve node ids via a subquery over
// `nodes`). AfterNodeInsert is the hook for per-node mirror writes.
type PromotionSink interface {
	// Prepare prepares any statements against the transaction. Called once per
	// Promote, before the per-file loop, so statements are reused per row.
	Prepare(ctx context.Context, tx *sql.Tx) error

	// BeforeNodeDelete runs once per file, inside the tx, BEFORE the store
	// deletes the old node rows for that file. branch and repoID scope the
	// file. Sinks with no pre-delete work return nil.
	BeforeNodeDelete(ctx context.Context, tx *sql.Tx, branch, repoID, filePath string) error

	// AfterNodeInsert runs once per node, inside the tx, AFTER the node row has
	// been inserted.
	AfterNodeInsert(ctx context.Context, tx *sql.Tx, n nodeWrite, promotedAt int64) error
}

// embedRefSink mirrors each promoted node into node_embedding_refs as
// state='pending'. node_id is the PK: re-promoting a node resets its embedding
// state back to pending so the embedder worker re-embeds it. content_hash is
// NULL until the worker computes the embedding bytes.
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

// BeforeNodeDelete is a no-op: embed-ref keying is per node_id, not per file.
func (s *embedRefSink) BeforeNodeDelete(context.Context, *sql.Tx, string, string, string) error {
	return nil
}

func (s *embedRefSink) AfterNodeInsert(ctx context.Context, _ *sql.Tx, n nodeWrite, promotedAt int64) error {
	_, err := s.upsert.ExecContext(ctx, n.NodeID, promotedAt)
	return err
}

// ftsSink keeps the node_fts_words / node_fts_trigrams virtual tables in sync
// with the promoted node rows. node_fts_words holds the pre-tokenised
// camelCase/snake_case/`.`/`::`-split form for unicode61; node_fts_trigrams
// holds the raw concatenated text for FTS5's trigram tokenizer. Both inserts
// live inside the promotion tx so they commit atomically with the parent node.
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
	// FTS5 allows DELETE WHERE-clauses against UNINDEXED columns. The prepared
	// per-node deletes are a defensive idempotency net — a re-promote within
	// the same tx would otherwise duplicate FTS rows for surviving nodes.
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

// BeforeNodeDelete clears FTS rows for every node that currently lives in this
// file BEFORE the store deletes the node rows themselves. The node_id IN
// (SELECT ...) subquery must observe the still-present rows, so this MUST run
// before the per-file node DELETE. Doing it this way keeps the FTS in sync even
// for nodes the new parse no longer produces (file shrank / symbol removed).
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

// AfterNodeInsert writes the FTS rows for one node: DELETE any prior row for
// this node_id, then INSERT. raw = "<kind> <symbol_path>" — what the trigram
// tokenizer sees. The file path is intentionally omitted: it tends to be long,
// noisy, and orthogonal to symbol-level lookup. words = tokenize.Symbol of the
// same — what unicode61 sees after camelCase/snake_case/`::`/`.` splits.
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
