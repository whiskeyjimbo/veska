package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// VectorStorage is the port for storing and querying vector embeddings.
type VectorStorage interface {
	// UpsertEmbeddings inserts or updates a batch of embedding rows. Rows with
	// duplicate NodeIDs replace existing entries.
	UpsertEmbeddings(ctx context.Context, repoID, branch string, batch []domain.EmbeddingRow) error

	// Search returns the k nearest neighbours to vec, optionally constrained by filter.
	// Results are sorted by score descending. If fewer than k vectors are indexed,
	// all stored vectors are returned.
	Search(ctx context.Context, repoID, branch string, vec []float32, k int, filter domain.VectorFilter) ([]domain.SearchHit, error)

	// Reindex rebuilds all stored embeddings. Implementations may treat this
	// as a no-op if the backing store handles quantization internally.
	Reindex(ctx context.Context, repoID string, modelID string) error

	LookupContentHashes(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error)
}
