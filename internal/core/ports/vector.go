// Package ports defines the interface contracts for the engram solov2 infrastructure.
package ports

import (
	"context"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
)

// VectorStorage is the port for storing and querying vector embeddings.
// Implementations are provided by infrastructure adapters (e.g. usearch, qdrant).
type VectorStorage interface {
	// UpsertEmbeddings inserts or updates a batch of embedding rows for the given
	// repository and branch. Rows with duplicate NodeIDs replace existing entries.
	UpsertEmbeddings(ctx context.Context, repoID, branch string, batch []domain.EmbeddingRow) error

	// Search returns the k nearest neighbours to vec, optionally constrained by filter.
	// Results are sorted by score descending. If fewer than k vectors are indexed,
	// all stored vectors are returned.
	Search(ctx context.Context, repoID, branch string, vec []float32, k int, filter domain.Filter) ([]domain.Hit, error)

	// Reindex rebuilds all stored embeddings using the specified model. Implementations
	// may treat this as a no-op if the backing store handles quantization internally.
	Reindex(ctx context.Context, repoID string, modelID string) error

	// LookupContentHashes returns a map of nodeID → contentHash for the given set of
	// node IDs in the specified repository and branch.
	LookupContentHashes(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error)
}
