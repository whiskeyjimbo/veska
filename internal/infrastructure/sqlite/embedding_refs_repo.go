// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlite contains SQLite-backed adapters for the veska ports layer.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// EmbeddingRefsRepo implements ports.EmbeddingRefRepo. It uses separate read and
// write handles to prevent long-running poll loops from contending with the
// single writer connection on which all writes are serialized.
type EmbeddingRefsRepo struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// NewEmbeddingRefsRepo constructs an EmbeddingRefsRepo. The writeDB parameter
// must be the singleton single-writer handle established by sqlite.Open to
// prevent write serialization issues.
func NewEmbeddingRefsRepo(readDB, writeDB *sql.DB) *EmbeddingRefsRepo {
	return &EmbeddingRefsRepo{readDB: readDB, writeDB: writeDB}
}

// FetchPending returns up to limit pending references ordered by enqueued time and
// node ID for deterministic batching. The returned Text field is a deterministic
// projection that includes the file path and language to prevent content-addressed
// deduplication from collapsing identical symbols in different files.
func (r *EmbeddingRefsRepo) FetchPending(ctx context.Context, limit int) ([]ports.PendingEmbedRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.readDB.QueryContext(ctx, `
		SELECT r.node_id, n.repo_id, n.branch, n.symbol_path, n.kind, n.file_path, n.language, n.snippet
		FROM node_embedding_refs r
		JOIN nodes n ON n.node_id = r.node_id
		WHERE r.state = 'pending'
		ORDER BY r.enqueued_at, r.node_id
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("embedding_refs: fetch pending: %w", err)
	}
	defer rows.Close()

	out := make([]ports.PendingEmbedRef, 0, limit)
	for rows.Next() {
		var p ports.PendingEmbedRef
		var snippet sql.NullString
		if err := rows.Scan(&p.NodeID, &p.RepoID, &p.Branch, &p.SymbolPath, &p.Kind, &p.FilePath, &p.Language, &snippet); err != nil {
			return nil, fmt.Errorf("embedding_refs: scan pending: %w", err)
		}
		p.Text = embedText(p.Kind, p.SymbolPath, p.FilePath, p.Language, snippet.String)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding_refs: iterate pending: %w", err)
	}
	return out, nil
}

// embedText delegates projection logic to domain.EmbedText to ensure the evaluation
// harness measures variants against production behavior. The snippet-based variant
// is used because benchmark evaluations showed it roughly doubles recall@10 over the
// baseline, degrading gracefully to the baseline projection if the snippet is empty.
func embedText(kind, symbolPath, filePath, language, snippet string) string {
	return domain.EmbedText(domain.EmbedTextInput{
		Kind:       kind,
		SymbolPath: symbolPath,
		FilePath:   filePath,
		Language:   language,
		Snippet:    snippet,
	}, domain.EmbedVariantSnippet)
}

// CountPending returns the count of pending references with a backing node. The
// EXISTS guard prevents orphaned references (which occur because
// node_embedding_refs lacks a foreign key to nodes due to the composite primary
// key on nodes) from inflating the backlog status metrics. Using EXISTS instead of
// a JOIN keeps the count strictly 1:1 if a node ID spans branches.
func (r *EmbeddingRefsRepo) CountPending(ctx context.Context) (int, error) {
	var n int
	err := r.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_embedding_refs r
		 WHERE r.state = 'pending'
		   AND EXISTS (SELECT 1 FROM nodes n WHERE n.node_id = r.node_id)`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("embedding_refs: count pending: %w", err)
	}
	return n, nil
}

// MarkReady atomically inserts the content-addressed embedding and marks the
// reference as ready. The embedding insert uses ON CONFLICT DO NOTHING to avoid
// overwriting existing equivalent embeddings, while updating the reference with
// the correct hash.
func (r *EmbeddingRefsRepo) MarkReady(
	ctx context.Context,
	nodeID, contentHash, modelID string,
	dim int,
	embedding []byte,
	at time.Time,
) error {
	tx, err := r.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("embedding_refs: begin tx: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_embeddings(content_hash, model, dim, embedding, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO NOTHING`,
		contentHash, modelID, dim, embedding, at.UnixMilli(),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("embedding_refs: insert node_embeddings: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE node_embedding_refs
		SET state='ready', content_hash=?, embedded_at=?
		WHERE node_id=?`,
		contentHash, at.UnixMilli(), nodeID,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("embedding_refs: update ref: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("embedding_refs: commit: %w", err)
	}
	return nil
}

// MarkAttemptFailed increments the attempt counter and transitions the state to
// failed if maxAttempts is reached. This update is executed as a single query to
// prevent concurrent fetch operations from observing a pending row with exceeded
// attempts. Rows not in a pending state are unaffected.
func (r *EmbeddingRefsRepo) MarkAttemptFailed(ctx context.Context, nodeID string, maxAttempts int) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	_, err := r.writeDB.ExecContext(ctx, `
		UPDATE node_embedding_refs
		SET attempts = attempts + 1,
		    state = CASE
		        WHEN attempts + 1 >= ? THEN 'failed'
		        ELSE state
		    END
		WHERE node_id = ? AND state = 'pending'`,
		maxAttempts, nodeID,
	)
	if err != nil {
		return fmt.Errorf("embedding_refs: mark attempt failed: %w", err)
	}
	return nil
}

// CountByState returns row counts grouped by state. Non-existent states are
// pre-populated with zero values so callers can index the returned map directly.
func (r *EmbeddingRefsRepo) CountByState(ctx context.Context) (map[string]int, error) {
	out := map[string]int{"pending": 0, "ready": 0, "failed": 0}
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM node_embedding_refs GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("embedding_refs: count by state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("embedding_refs: scan count: %w", err)
		}
		out[state] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding_refs: iterate counts: %w", err)
	}
	return out, nil
}

// LookupExisting checks if an equivalent content-addressed embedding already
// exists for the given model and text. A hit allows the caller to skip calling the
// embedding provider.
func (r *EmbeddingRefsRepo) LookupExisting(ctx context.Context, contentHash string) ([]byte, int, bool, error) {
	var (
		blob []byte
		dim  int
	)
	err := r.readDB.QueryRowContext(ctx,
		`SELECT embedding, dim FROM node_embeddings WHERE content_hash = ?`,
		contentHash,
	).Scan(&blob, &dim)
	switch {
	case err == sql.ErrNoRows:
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, fmt.Errorf("embedding_refs: lookup existing: %w", err)
	}
	return blob, dim, true, nil
}

// Reuse updates a pending reference to ready using an existing embedding hash. It
// targets only pending rows to prevent concurrent callers from regressing a
// reference that has already transitioned to another state.
func (r *EmbeddingRefsRepo) Reuse(ctx context.Context, nodeID, contentHash string, at time.Time) error {
	_, err := r.writeDB.ExecContext(ctx, `
		UPDATE node_embedding_refs
		SET state='ready', content_hash=?, embedded_at=?
		WHERE node_id=? AND state='pending'`,
		contentHash, at.UnixMilli(), nodeID,
	)
	if err != nil {
		return fmt.Errorf("embedding_refs: reuse: %w", err)
	}
	return nil
}

// ContentHashForNode returns the embedding hash for a node scoped to a specific
// repository and branch. The JOIN against nodes prevents stale or cross-repository
// node IDs from leaking hashes. If the node or reference is missing or not ready,
// it returns an empty string without an error.
func (r *EmbeddingRefsRepo) ContentHashForNode(ctx context.Context, repoID, branch, nodeID string) (string, bool, error) {
	var (
		hash  sql.NullString
		state string
	)
	err := r.readDB.QueryRowContext(ctx, `
		SELECT r.content_hash, r.state
		FROM node_embedding_refs r
		JOIN nodes n ON n.node_id = r.node_id
		WHERE r.node_id = ? AND n.repo_id = ? AND n.branch = ?`,
		nodeID, repoID, branch,
	).Scan(&hash, &state)
	switch {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("embedding_refs: content hash for node: %w", err)
	}
	if state != "ready" || !hash.Valid || hash.String == "" {
		return "", false, nil
	}
	return hash.String, true, nil
}

var _ ports.EmbeddingRefRepo = (*EmbeddingRefsRepo)(nil)
