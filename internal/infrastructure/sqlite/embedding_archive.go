// Package sqlite contains SQLite-backed adapters for the veska ports layer.
// This file implements the store-wide embedding maintenance operations the
// embedder boot path depends on (embedder.EmbeddingLoader plus the model-switch
// requeue) against the node_embeddings and node_embedding_refs tables.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
)

// EmbeddingArchive is the SQLite-backed adapter for store-wide embedding
// maintenance: loading persisted ready embeddings for vector rehydration and
// requeuing every ref under a newly-elected model. It satisfies
// embedder.EmbeddingLoader.
//
// Like EmbeddingRefsRepo it holds separate read and write handles so the
// boot-time rehydration read never contends with the single writer connection.
type EmbeddingArchive struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// NewEmbeddingArchive constructs an EmbeddingArchive over the given handles,
// typically Pools.ReadDB and Pools.Write.
func NewEmbeddingArchive(readDB, writeDB *sql.DB) *EmbeddingArchive {
	return &EmbeddingArchive{readDB: readDB, writeDB: writeDB}
}

// LoadReadyEmbeddings returns every ready embedding ref joined to its persisted
// vector blob, for rehydration into a VectorStorage after a daemon restart
// (solov2-249). Only refs with state='ready' and a non-NULL content_hash are
// returned; pending/failed refs carry no vector yet.
func (a *EmbeddingArchive) LoadReadyEmbeddings(ctx context.Context) ([]embedder.ReadyEmbeddingRow, error) {
	const q = `
		SELECT n.repo_id, n.branch, r.node_id, r.content_hash, e.model, e.dim, e.embedding
		FROM node_embedding_refs r
		JOIN nodes n            ON n.node_id = r.node_id
		JOIN node_embeddings e  ON e.content_hash = r.content_hash
		WHERE r.state = 'ready' AND r.content_hash IS NOT NULL
	`
	rows, err := a.readDB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("load ready embeddings: query: %w", err)
	}
	defer rows.Close()

	var out []embedder.ReadyEmbeddingRow
	for rows.Next() {
		var row embedder.ReadyEmbeddingRow
		if err := rows.Scan(&row.RepoID, &row.Branch, &row.NodeID, &row.ContentHash, &row.ModelID, &row.Dim, &row.Blob); err != nil {
			return nil, fmt.Errorf("load ready embeddings: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load ready embeddings: iterate: %w", err)
	}
	return out, nil
}

// RequeueAllUnderNewModel wipes the content-addressed embedding store and
// resets every embedding-ref to pending, so the embedder worker re-embeds all
// promoted nodes under the currently-elected model.
//
// Why needed (solov2-fz8): node_embeddings is keyed by content_hash with
// ON CONFLICT(content_hash) DO NOTHING — re-embedding the same content under
// a different model would otherwise be a no-op and keep the old-model vector.
// And the sqlite-vec store is in-memory, rehydrated from node_embeddings at
// boot — so this MUST run before vector rehydration to start the store empty.
//
// Returns the number of refs flipped back to pending so the daemon log can
// surface "auto-reindex N nodes".
func (a *EmbeddingArchive) RequeueAllUnderNewModel(ctx context.Context) (int64, error) {
	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("requeue embeddings: begin: %w", err)
	}
	// Reset refs FIRST: node_embedding_refs.content_hash has a FK into
	// node_embeddings, so the embeddings can only be cleared after every ref
	// has dropped its reference.
	res, err := tx.ExecContext(ctx, `UPDATE node_embedding_refs
		SET state = 'pending', content_hash = NULL, embedded_at = NULL, attempts = 0`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("requeue embeddings: reset refs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_embeddings`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("requeue embeddings: clear node_embeddings: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("requeue embeddings: commit: %w", err)
	}
	return n, nil
}
