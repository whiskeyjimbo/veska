// Package sqlite contains SQLite-backed adapters for the veska ports layer.
// This file implements ports.EmbeddingRefRepo against the node_embedding_refs
// and node_embeddings tables introduced by migration 0004.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

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
// The Text field is a deterministic projection: "<kind> <symbol_path>".
// m3.02.1 intentionally keeps this trivial; m3.02.4 (dedupe) and future tasks
// may swap in a richer projection without schema change.
func (r *EmbeddingRefsRepo) FetchPending(ctx context.Context, limit int) ([]ports.PendingEmbedRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.readDB.QueryContext(ctx, `
		SELECT r.node_id, n.repo_id, n.branch, n.symbol_path, n.kind
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
		if err := rows.Scan(&p.NodeID, &p.RepoID, &p.Branch, &p.SymbolPath, &p.Kind); err != nil {
			return nil, fmt.Errorf("embedding_refs: scan pending: %w", err)
		}
		p.Text = p.Kind + " " + p.SymbolPath
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding_refs: iterate pending: %w", err)
	}
	return out, nil
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

// Compile-time check.
var _ ports.EmbeddingRefRepo = (*EmbeddingRefsRepo)(nil)
