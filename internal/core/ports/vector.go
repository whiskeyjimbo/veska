// SPDX-License-Identifier: AGPL-3.0-only

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

	// Search returns the k nearest neighbors to vec, optionally constrained by filter.
	// Results are sorted by score descending. If fewer than k vectors are indexed,
	// all stored vectors are returned.
	Search(ctx context.Context, repoID, branch string, vec []float32, k int, filter domain.VectorFilter) ([]domain.SearchHit, error)

	// Reindex rebuilds all stored embeddings. Implementations may treat this
	// as a no-op if the backing store handles quantization internally.
	Reindex(ctx context.Context, repoID string, modelID string) error

	// DeleteNodes removes the given nodes' vectors from (repoID, branch) across
	// every model partition. Called when a re-promote drops symbols, so their
	// vectors don't linger as stale scan candidates until a daemon restart.
	// Unknown node_ids are ignored; an empty slice is a no-op.
	DeleteNodes(ctx context.Context, repoID, branch string, nodeIDs []string) error

	LookupContentHashes(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error)
}
