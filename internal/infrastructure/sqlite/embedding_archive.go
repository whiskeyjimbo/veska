// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlite contains SQLite-backed adapters for the veska ports layer.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
)

// EmbeddingArchive manages store-wide embedding maintenance. It uses separate read
// and write handles to prevent boot-time rehydration reads from contending with the
// single writer connection.
type EmbeddingArchive struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// NewEmbeddingArchive constructs an EmbeddingArchive over the given handles.
func NewEmbeddingArchive(readDB, writeDB *sql.DB) *EmbeddingArchive {
	return &EmbeddingArchive{readDB: readDB, writeDB: writeDB}
}

// LoadReadyEmbeddings returns ready embedding references and their vector blobs
// for vector storage rehydration. Pending or failed references are excluded
// since they do not yet carry vector data.
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

// RequeueAllUnderNewModel resets all embedding references to pending and clears
// the embedding store to force re-embedding under a new model. This reset is
// necessary because node_embeddings is keyed by content_hash with ON CONFLICT DO
// NOTHING, which would otherwise keep the old-model vector when embedding
// unchanged content under a new model. This must execute before boot-time vector
// rehydration begins.
func (a *EmbeddingArchive) RequeueAllUnderNewModel(ctx context.Context) (int64, error) {
	tx, err := a.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("requeue embeddings: begin: %w", err)
	}
	// Reset references first because node_embedding_refs.content_hash has a
	// foreign key referencing node_embeddings, requiring reference handles
	// to be cleared before the embeddings can be deleted.
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
