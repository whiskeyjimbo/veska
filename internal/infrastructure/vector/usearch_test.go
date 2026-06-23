// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

const testDim = 768

// randVec generates a pseudo-random float32 vector of length testDim for testing.
func randVec(seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	v := make([]float32, testDim)
	for i := range v {
		v[i] = rng.Float32()
	}
	return v
}

func newStore(t *testing.T) *vector.UsearchStore {
	t.Helper()
	s, err := vector.NewUsearchStore()
	if err != nil {
		t.Fatalf("NewUsearchStore: %v", err)
	}
	t.Cleanup(func() { s.Destroy() })
	return s
}

// TestUpsertAndLookup verifies that inserting a batch of rows allows their content hashes to be successfully retrieved.
func TestUpsertAndLookup(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	const n = 10
	batch := make([]domain.EmbeddingRow, n)
	nodeIDs := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node-%d", i)
		batch[i] = domain.EmbeddingRow{
			NodeID:      id,
			ContentHash: fmt.Sprintf("hash-%d", i),
			ModelID:     "nomic-embed-text",
			Vector:      randVec(int64(i)),
		}
		nodeIDs[i] = id
	}

	if err := store.UpsertEmbeddings(ctx, "repo-1", "main", batch); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	hashes, err := store.LookupContentHashes(ctx, "repo-1", "main", nodeIDs)
	if err != nil {
		t.Fatalf("LookupContentHashes: %v", err)
	}
	if len(hashes) != n {
		t.Errorf("expected %d hashes, got %d", n, len(hashes))
	}
	for i, id := range nodeIDs {
		want := fmt.Sprintf("hash-%d", i)
		if got := hashes[id]; got != want {
			t.Errorf("nodeID %q: want hash %q, got %q", id, want, got)
		}
	}
}

// TestSearch verifies that searching the store returns the requested number of nearest neighbor hits with non-negative similarity scores.
// TestDeleteNodesRemovesFromSearch is the usearch half of solov2-524u: a
// deleted node must stop surfacing in Search and LookupContentHashes.
func TestDeleteNodesRemovesFromSearch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	batch := []domain.EmbeddingRow{
		{NodeID: "keep", ContentHash: "hk", ModelID: "nomic-embed-text", Vector: randVec(1)},
		{NodeID: "drop", ContentHash: "hd", ModelID: "nomic-embed-text", Vector: randVec(2)},
	}
	if err := store.UpsertEmbeddings(ctx, "repo-1", "main", batch); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	if err := store.DeleteNodes(ctx, "repo-1", "main", []string{"drop"}); err != nil {
		t.Fatalf("DeleteNodes: %v", err)
	}

	hits, err := store.Search(ctx, "repo-1", "main", randVec(2), 5, domain.VectorFilter{ModelID: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.NodeID == "drop" {
			t.Fatalf("drop still searchable after DeleteNodes: %+v", hits)
		}
	}
	hashes, err := store.LookupContentHashes(ctx, "repo-1", "main", []string{"keep", "drop"})
	if err != nil {
		t.Fatalf("LookupContentHashes: %v", err)
	}
	if _, ok := hashes["drop"]; ok {
		t.Fatalf("drop content hash still present after DeleteNodes")
	}
	if _, ok := hashes["keep"]; !ok {
		t.Fatalf("keep wrongly removed by DeleteNodes(drop)")
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	const n = 50
	batch := make([]domain.EmbeddingRow, n)
	for i := 0; i < n; i++ {
		batch[i] = domain.EmbeddingRow{
			NodeID:      fmt.Sprintf("node-%d", i),
			ContentHash: fmt.Sprintf("hash-%d", i),
			ModelID:     "nomic-embed-text",
			Vector:      randVec(int64(i + 100)),
		}
	}

	if err := store.UpsertEmbeddings(ctx, "repo-1", "main", batch); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	query := randVec(9999)
	hits, err := store.Search(ctx, "repo-1", "main", query, 5, domain.VectorFilter{ModelID: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 5 {
		t.Errorf("expected 5 hits, got %d", len(hits))
	}
	for _, h := range hits {
		if h.Score < 0 {
			t.Errorf("hit %q: score %f is negative", h.NodeID, h.Score)
		}
	}
}

// TestFilterModelID verifies that searching with a model identifier that does not match any indexed rows returns zero results.
func TestFilterModelID(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	batch := []domain.EmbeddingRow{
		{
			NodeID:      "node-a",
			ContentHash: "hash-a",
			ModelID:     "nomic-embed-text",
			Vector:      randVec(1),
		},
	}
	if err := store.UpsertEmbeddings(ctx, "repo-1", "main", batch); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	hits, err := store.Search(ctx, "repo-1", "main", randVec(2), 5, domain.VectorFilter{ModelID: "other-model"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits with mismatched modelID, got %d", len(hits))
	}
}

// TestReindex verifies that calling Reindex on UsearchStore returns a nil error.
func TestReindex(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if err := store.Reindex(ctx, "repo-1", "nomic-embed-text"); err != nil {
		t.Errorf("Reindex: expected nil, got %v", err)
	}
}

// TestUpsertUpdatesExistingNode verifies that inserting a row with a pre-existing node identifier replaces the existing row metadata and vector.
func TestUpsertUpdatesExistingNode(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	row := domain.EmbeddingRow{
		NodeID:      "node-x",
		ContentHash: "hash-v1",
		ModelID:     "nomic-embed-text",
		Vector:      randVec(10),
	}
	if err := store.UpsertEmbeddings(ctx, "repo-1", "main", []domain.EmbeddingRow{row}); err != nil {
		t.Fatalf("first UpsertEmbeddings: %v", err)
	}

	row.ContentHash = "hash-v2"
	row.Vector = randVec(11)
	if err := store.UpsertEmbeddings(ctx, "repo-1", "main", []domain.EmbeddingRow{row}); err != nil {
		t.Fatalf("second UpsertEmbeddings: %v", err)
	}

	hashes, err := store.LookupContentHashes(ctx, "repo-1", "main", []string{"node-x"})
	if err != nil {
		t.Fatalf("LookupContentHashes: %v", err)
	}
	if hashes["node-x"] != "hash-v2" {
		t.Errorf("expected updated hash hash-v2, got %q", hashes["node-x"])
	}
}
