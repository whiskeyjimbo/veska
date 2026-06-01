// Package sqlite contains SQLite-backed adapters for the veska ports layer.
// This file implements ports.EmbeddingRefRepo against the node_embedding_refs
// and node_embeddings tables introduced by migration 0004.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// EmbeddingRefsRepo is a SQLite-backed implementation of ports.EmbeddingRefRepo.
//
// It uses separate read and write handles so a long-running poll loop reading
// pending rows never contends with the single writer connection on which the
// daemon serialises all SQLite writes.
type EmbeddingRefsRepo struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// NewEmbeddingRefsRepo constructs an EmbeddingRefsRepo. The caller is
// responsible for ensuring writeDB is the singleton single-writer handle
// established by sqlite.Open.
func NewEmbeddingRefsRepo(readDB, writeDB *sql.DB) *EmbeddingRefsRepo {
	return &EmbeddingRefsRepo{readDB: readDB, writeDB: writeDB}
}

// FetchPending returns up to limit pending refs joined with the minimal node
// fields needed to embed. Rows are ordered by enqueued_at then node_id for
// deterministic batch composition.
//
// The Text field is a deterministic projection:
// "<kind> <symbol_path> <file_path> <language>" (empty trailing fields are
// omitted). file_path and language disambiguate otherwise-identical symbols so
// the content-addressed embedding dedup does not collapse genuinely-distinct
// nodes; re-promoting the same node in the same file still dedups.
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

// embedText builds the deterministic Embed-input projection for a node,
// joining the non-empty parts with a single space. kind and symbolPath are
// always present; filePath and language may be empty (the parser may leave
// language unset), and snippet may be empty when nodes.snippet is NULL.
//
// The projection logic itself lives in domain.EmbedText so the recall
// eval harness (tools/loadtest/recallprojection) measures projection
// variants against exactly what production emits. Production uses
// EmbedVariantSnippet: a faithful real-code recall sweep
// across veska, golang.org/x/mod and BurntSushi/toml — real source-body
// snippets, doc-comment queries — showed +snippet roughly doubles recall@10
// over baseline at flat p95. An empty snippet degrades gracefully:
// domain.EmbedText skips empty parts, so a NULL-snippet node yields exactly
// the baseline "<kind> <symbol_path> <file_path> <language>" projection.
func embedText(kind, symbolPath, filePath, language, snippet string) string {
	return domain.EmbedText(domain.EmbedTextInput{
		Kind:       kind,
		SymbolPath: symbolPath,
		FilePath:   filePath,
		Language:   language,
		Snippet:    snippet,
	}, domain.EmbedVariantSnippet)
}

// CountPending returns the count of state='pending' rows.
func (r *EmbeddingRefsRepo) CountPending(ctx context.Context) (int, error) {
	var n int
	err := r.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_embedding_refs WHERE state = 'pending'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("embedding_refs: count pending: %w", err)
	}
	return n, nil
}

// MarkReady upserts the content-addressed embedding bytes and updates the ref
// to state='ready' atomically.
//
// node_embeddings uses ON CONFLICT(content_hash) DO NOTHING so re-embedding
// the same content is a no-op for the bytes table; the ref row is still
// updated to reflect the (possibly new) content_hash for this node.
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

// MarkAttemptFailed bumps the per-row attempts counter and, when the new
// value reaches maxAttempts, atomically flips state to 'failed'. The
// bump-and-flip is a single UPDATE so a concurrent FetchPending cannot
// observe a row with attempts>=maxAttempts that is still 'pending'.
//
// Rows not in state='pending' (already 'ready' or 'failed') are left
// untouched. maxAttempts <= 0 is treated as 1 (any failure is fatal).
func (r *EmbeddingRefsRepo) MarkAttemptFailed(ctx context.Context, nodeID string, maxAttempts int) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	// CASE: only flip to 'failed' when the *new* attempts value (attempts+1)
	// is >= maxAttempts. Otherwise keep state='pending' so the next tick
	// picks the row back up.
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

// CountByState returns row counts for {pending, ready, failed}. Missing
// states are returned with a 0 value so callers can index without
// ok-checks.
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

// LookupExisting probes node_embeddings for a row keyed by contentHash. A hit
// returns (bytes, dim, true, nil); a miss returns (nil, 0, false, nil). The
// hash is content-addressed on the embed INPUT (modelID + embed_text), so a
// hit means an equivalent Embed call has already produced these bytes for
// this model — the worker can skip the provider call.
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

// Reuse flips a pending ref to state='ready' against an existing
// content_hash WITHOUT touching node_embeddings. Used by the dedup fast-path
// when LookupExisting reported a hit. Rows not in state='pending' are left
// alone so a racy second caller observing the same hit cannot regress a
// row already marked ready.
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

// ContentHashForNode returns the content_hash and ready flag for nodeID
// scoped to (repoID, branch). A JOIN against nodes enforces the scope so a
// stale or cross-repo node_id cannot leak a hash out of its origin tree.
//
// ready=true requires BOTH state='ready' AND a non-NULL content_hash; either
// alone returns ready=false. err=nil with ready=false also covers the
// "no such ref" and "no such node" cases — the caller decides whether to
// skip or escalate.
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

// Compile-time check.
var _ ports.EmbeddingRefRepo = (*EmbeddingRefsRepo)(nil)
