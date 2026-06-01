package embedder

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ReadyEmbeddingRow is one persisted, ready embedding awaiting rehydration into
// a VectorStorage. Blob is the little-endian float32 packing of Dim floats, as
// stored in node_embeddings.embedding. It carries no SQL types so it crosses
// the application/infrastructure boundary cleanly.
type ReadyEmbeddingRow struct {
	RepoID      string
	Branch      string
	NodeID      string
	ContentHash string
	ModelID     string
	Dim         int
	Blob        []byte
}

// EmbeddingLoader yields the persisted ready embeddings a daemon restart must
// reload into VectorStorage. The SQL implementation lives in
// internal/infrastructure/sqlite (EmbeddingArchive) — keeping RehydrateVectors
// free of database/sql, mirroring how the Promoter delegates to PromotionStore.
type EmbeddingLoader interface {
	LoadReadyEmbeddings(ctx context.Context) ([]ReadyEmbeddingRow, error)
}

// RehydrateVectors rebuilds the VectorStorage from the durable node_embeddings
// + node_embedding_refs join (exposed by the EmbeddingLoader). The memory backend is an
// in-memory backend and the usearch backend's persisted indexes are at-rest
// only — neither survives a daemon restart with its pre-restart contents intact
// . Without this rehydration call at Daemon.Start, semantic search
// returns ≤ 0 hits until something forces a re-embed, which can only happen via
// a content_hash change.
//
// Idempotent: UpsertEmbeddings is keyed by node_id within (repo, branch),
// so calling this multiple times is safe.
//
// Returns the per-(repo, branch) row count loaded so the daemon log can
// confirm a non-zero hydrate at start.
func RehydrateVectors(
	ctx context.Context,
	loader EmbeddingLoader,
	vectors ports.VectorStorage,
) (map[string]int, error) {
	if loader == nil {
		return nil, fmt.Errorf("rehydrate: loader is nil: %w", ErrMissingDependency)
	}
	if vectors == nil {
		return nil, fmt.Errorf("rehydrate: vectors is nil: %w", ErrMissingDependency)
	}

	rows, err := loader.LoadReadyEmbeddings(ctx)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: load: %w", err)
	}

	// Bucket per (repo, branch) — VectorStorage.UpsertEmbeddings is scoped to
	// (repo, branch), and each row carries its own ModelID for the usearch
	// backend's partition key.
	type bucketKey struct{ repo, branch string }
	buckets := make(map[bucketKey][]domain.EmbeddingRow)

	for _, row := range rows {
		vec := veccodec.DecodeFloat32LE(row.Blob, row.Dim)
		if len(vec) == 0 {
			continue
		}
		k := bucketKey{repo: row.RepoID, branch: row.Branch}
		buckets[k] = append(buckets[k], domain.EmbeddingRow{
			NodeID:      row.NodeID,
			ContentHash: row.ContentHash,
			ModelID:     row.ModelID,
			Vector:      vec,
		})
	}

	counts := make(map[string]int, len(buckets))
	for k, batch := range buckets {
		if uerr := vectors.UpsertEmbeddings(ctx, k.repo, k.branch, batch); uerr != nil {
			// Non-fatal: return what we have so the caller may continue.
			return counts, fmt.Errorf("rehydrate: upsert %s/%s: %w", k.repo, k.branch, uerr)
		}
		counts[k.repo+"@"+k.branch] = len(batch)
	}
	return counts, nil
}
