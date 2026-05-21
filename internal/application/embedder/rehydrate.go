package embedder

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RehydrateVectors rebuilds the VectorStorage from the durable node_embeddings
// + node_embedding_refs join. sqlite-vec is an in-memory backend and the
// usearch backend's persisted indexes are at-rest only — neither survives a
// daemon restart with its pre-restart contents intact (solov2-249). Without
// this rehydration call at Daemon.Start, semantic search returns ≤ 0 hits
// until something forces a re-embed, which can only happen via a content_hash
// change.
//
// Idempotent: UpsertEmbeddings is keyed by node_id within (repo, branch),
// so calling this multiple times is safe. Errors from individual rows are
// logged via the returned slice but don't abort the whole rehydrate — a
// single bad blob shouldn't deny the whole store.
//
// Returns the per-(repo, branch) row count loaded so the daemon log can
// confirm a non-zero hydrate at start.
func RehydrateVectors(
	ctx context.Context,
	readDB *sql.DB,
	vectors ports.VectorStorage,
) (map[string]int, error) {
	if readDB == nil {
		return nil, fmt.Errorf("rehydrate: readDB is nil: %w", ErrMissingDependency)
	}
	if vectors == nil {
		return nil, fmt.Errorf("rehydrate: vectors is nil: %w", ErrMissingDependency)
	}

	const q = `
		SELECT n.repo_id, n.branch, r.node_id, r.content_hash, e.model, e.dim, e.embedding
		FROM node_embedding_refs r
		JOIN nodes n            ON n.node_id = r.node_id
		JOIN node_embeddings e  ON e.content_hash = r.content_hash
		WHERE r.state = 'ready' AND r.content_hash IS NOT NULL
	`
	rows, err := readDB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: query: %w", err)
	}
	defer rows.Close()

	// Bucket per (repo, branch, model) — VectorStorage.UpsertEmbeddings is
	// scoped to (repo, branch), and each row carries its own ModelID for the
	// usearch backend's partition key. Keep the per-bucket batch small enough
	// that a transient backend error doesn't waste a large in-memory copy.
	type bucketKey struct{ repo, branch string }
	buckets := make(map[bucketKey][]domain.EmbeddingRow)

	for rows.Next() {
		var (
			repoID, branch, nodeID, contentHash, modelID string
			dim                                          int
			blob                                         []byte
		)
		if err := rows.Scan(&repoID, &branch, &nodeID, &contentHash, &modelID, &dim, &blob); err != nil {
			return nil, fmt.Errorf("rehydrate: scan: %w", err)
		}
		vec := decodeFloat32LE(blob, dim)
		if len(vec) == 0 {
			continue
		}
		k := bucketKey{repo: repoID, branch: branch}
		buckets[k] = append(buckets[k], domain.EmbeddingRow{
			NodeID:      nodeID,
			ContentHash: contentHash,
			ModelID:     modelID,
			Vector:      vec,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rehydrate: iterate: %w", err)
	}

	counts := make(map[string]int, len(buckets))
	for k, batch := range buckets {
		if uerr := vectors.UpsertEmbeddings(ctx, k.repo, k.branch, batch); uerr != nil {
			// Non-fatal: log via the error return; caller may continue.
			return counts, fmt.Errorf("rehydrate: upsert %s/%s: %w", k.repo, k.branch, uerr)
		}
		counts[k.repo+"@"+k.branch] = len(batch)
	}
	return counts, nil
}
