//go:build hnsw_native

package vector_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// TestSaveLoadRoundTrip inserts 50 vectors, runs 5 hold-out queries on the original
// store, saves to a temp directory, loads into a fresh store, then asserts that the
// same 5 queries return identical top-5 nodeID lists in the same order.
func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Build store with 50 vectors
	store := newStore(t)

	const n = 50
	batch := make([]domain.EmbeddingRow, n)
	for i := 0; i < n; i++ {
		batch[i] = domain.EmbeddingRow{
			NodeID:      fmt.Sprintf("persist-node-%d", i),
			ContentHash: fmt.Sprintf("persist-hash-%d", i),
			ModelID:     "nomic-embed-text",
			Vector:      randVec(int64(i + 200)),
		}
	}
	if err := store.UpsertEmbeddings(ctx, "r1", "main", batch); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	// Run 5 hold-out queries on original store
	querySeeds := []int64{5001, 5002, 5003, 5004, 5005}
	filter := domain.VectorFilter{ModelID: "nomic-embed-text"}

	originalResults := make([][]string, len(querySeeds))
	for qi, seed := range querySeeds {
		hits, err := store.Search(ctx, "r1", "main", randVec(seed), 5, filter)
		if err != nil {
			t.Fatalf("Search (original) query %d: %v", qi, err)
		}
		if len(hits) != 5 {
			t.Fatalf("Search (original) query %d: expected 5 hits, got %d", qi, len(hits))
		}
		nodeIDs := make([]string, len(hits))
		for i, h := range hits {
			nodeIDs[i] = h.NodeID
		}
		originalResults[qi] = nodeIDs
	}

	// Save to temp directory
	tmpDir := t.TempDir()
	if err := store.Save(tmpDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load into fresh store
	store2, err := vector.NewUsearchStore()
	if err != nil {
		t.Fatalf("NewUsearchStore (store2): %v", err)
	}
	t.Cleanup(func() { store2.Destroy() })

	if err := store2.Load(tmpDir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Run same 5 queries on store2 and assert exact match
	for qi, seed := range querySeeds {
		hits, err := store2.Search(ctx, "r1", "main", randVec(seed), 5, filter)
		if err != nil {
			t.Fatalf("Search (store2) query %d: %v", qi, err)
		}
		if len(hits) != 5 {
			t.Fatalf("Search (store2) query %d: expected 5 hits, got %d", qi, len(hits))
		}
		for i, h := range hits {
			want := originalResults[qi][i]
			if h.NodeID != want {
				t.Errorf("query %d position %d: want nodeID %q, got %q", qi, i, want, h.NodeID)
			}
		}
	}
}

// TestSaveLoadRoundTripHyphenatedKeys verifies that key fields containing hyphens
// (e.g. repoID="my-repo") round-trip correctly through Save and Load.
func TestSaveLoadRoundTripHyphenatedKeys(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	batch := []domain.EmbeddingRow{
		{
			NodeID:      "node-0",
			ContentHash: "hash-0",
			ModelID:     "nomic-embed-text",
			Vector:      randVec(7001),
		},
	}
	if err := store.UpsertEmbeddings(ctx, "my-repo", "feature/foo-bar", batch); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	tmpDir := t.TempDir()
	if err := store.Save(tmpDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	store2, err := vector.NewUsearchStore()
	if err != nil {
		t.Fatalf("NewUsearchStore: %v", err)
	}
	t.Cleanup(func() { store2.Destroy() })

	if err := store2.Load(tmpDir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	hashes, err := store2.LookupContentHashes(ctx, "my-repo", "feature/foo-bar", []string{"node-0"})
	if err != nil {
		t.Fatalf("LookupContentHashes: %v", err)
	}
	if hashes["node-0"] != "hash-0" {
		t.Errorf("expected hash-0, got %q", hashes["node-0"])
	}
}

// TestSaveEmptyStore verifies that Save on an empty store does not error.
func TestSaveEmptyStore(t *testing.T) {
	store := newStore(t)
	tmpDir := t.TempDir()
	if err := store.Save(tmpDir); err != nil {
		t.Fatalf("Save (empty store): %v", err)
	}
}

// TestLoadEmptyDir verifies that Load from an empty directory does not error.
func TestLoadEmptyDir(t *testing.T) {
	store := newStore(t)
	tmpDir := t.TempDir()
	if err := store.Load(tmpDir); err != nil {
		t.Fatalf("Load (empty dir): %v", err)
	}
}
