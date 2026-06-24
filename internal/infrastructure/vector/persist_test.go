// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// cleanMarkerName is the on-disk clean-shutdown marker filename. Mirrors the
// unexported vector.cleanShutdownMarker; kept in sync by these tests.
const cleanMarkerName = "vec-clean-shutdown"

// TestSaveLoadRoundTrip inserts 50 vectors, runs 5 hold-out queries on the original
// store, saves to a temporary directory, loads into a fresh store, and asserts that the
// same 5 queries return identical top-5 nodeID lists in the same order.
func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Build a store populated with 50 vectors.
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

	// Run 5 hold-out queries on the original store.
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

	// Save the original store to a temporary directory.
	tmpDir := t.TempDir()
	if err := store.Save(tmpDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load the saved index into a fresh store instance.
	store2, err := vector.NewUsearchStore(vector.Options{})
	if err != nil {
		t.Fatalf("NewUsearchStore (store2): %v", err)
	}
	t.Cleanup(func() { store2.Destroy() })

	if err := store2.Load(tmpDir); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Run the same 5 queries on the new store and assert that the results match exactly.
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

// TestOpen_CleanShutdownReloadsAndConsumesMarker verifies the fast-boot path:
// after Save drops the clean-shutdown marker, Open reloads the snapshot, reports
// HydratedFromDisk()=true, consumes the marker, and serves the loaded vectors.
func TestOpen_CleanShutdownReloadsAndConsumesMarker(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.UpsertEmbeddings(ctx, "r1", "main", []domain.EmbeddingRow{
		{NodeID: "node-0", ContentHash: "hash-0", ModelID: "nomic-embed-text", Vector: randVec(9100)},
	}); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}
	dir := t.TempDir()
	if err := store.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	markerPath := filepath.Join(dir, cleanMarkerName)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected clean-shutdown marker after Save: %v", err)
	}

	reopened, err := vector.Open(dir, vector.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { reopened.(interface{ Destroy() }).Destroy() })

	if h, ok := reopened.(interface{ HydratedFromDisk() bool }); !ok || !h.HydratedFromDisk() {
		t.Fatalf("expected HydratedFromDisk()=true after clean-shutdown reopen")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected marker consumed by Open, stat err=%v", err)
	}
	hits, err := reopened.Search(ctx, "r1", "main", randVec(9100), 1, domain.VectorFilter{ModelID: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit from reloaded index, got %d", len(hits))
	}
}

// TestOpen_NoMarkerSkipsLoad verifies the crash path: with sidecars present but
// no marker, Open ignores the snapshot (HydratedFromDisk()=false, empty store)
// so the daemon rebuilds from node_embeddings - which is what keeps vectors for
// since-deleted nodes out of the index.
func TestOpen_NoMarkerSkipsLoad(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.UpsertEmbeddings(ctx, "r1", "main", []domain.EmbeddingRow{
		{NodeID: "node-0", ContentHash: "hash-0", ModelID: "nomic-embed-text", Vector: randVec(9200)},
	}); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}
	dir := t.TempDir()
	if err := store.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, cleanMarkerName)); err != nil {
		t.Fatalf("simulate crash (remove marker): %v", err)
	}

	reopened, err := vector.Open(dir, vector.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { reopened.(interface{ Destroy() }).Destroy() })

	if h, ok := reopened.(interface{ HydratedFromDisk() bool }); !ok || h.HydratedFromDisk() {
		t.Fatalf("expected HydratedFromDisk()=false without marker")
	}
	hits, err := reopened.Search(ctx, "r1", "main", randVec(9200), 1, domain.VectorFilter{ModelID: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits (snapshot ignored without marker), got %d", len(hits))
	}
}

// TestOpen_CorruptSnapshotFallsBackNotFatal verifies a marker claiming a clean
// shutdown but pointing at an unparseable sidecar does not brick startup: Open
// drops the partial load and returns an empty store for SQL rebuild.
func TestOpen_CorruptSnapshotFallsBackNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, cleanMarkerName), nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vec-bad|x|y.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write bad sidecar: %v", err)
	}
	reopened, err := vector.Open(dir, vector.Options{})
	if err != nil {
		t.Fatalf("Open must not be fatal on a corrupt snapshot: %v", err)
	}
	t.Cleanup(func() { reopened.(interface{ Destroy() }).Destroy() })
	if h, ok := reopened.(interface{ HydratedFromDisk() bool }); !ok || h.HydratedFromDisk() {
		t.Fatalf("expected HydratedFromDisk()=false after corrupt-snapshot fallback")
	}
}

// TestSaveLoadRoundTripHyphenatedKeys verifies that key fields containing hyphens
// (such as repoID="my-repo") round-trip correctly through Save and Load.
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

	store2, err := vector.NewUsearchStore(vector.Options{})
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

// TestSaveEmptyStore verifies that calling Save on an empty store does not return an error.
func TestSaveEmptyStore(t *testing.T) {
	store := newStore(t)
	tmpDir := t.TempDir()
	if err := store.Save(tmpDir); err != nil {
		t.Fatalf("Save (empty store): %v", err)
	}
}

// TestLoadEmptyDir verifies that calling Load on an empty directory does not return an error.
func TestLoadEmptyDir(t *testing.T) {
	store := newStore(t)
	tmpDir := t.TempDir()
	if err := store.Load(tmpDir); err != nil {
		t.Fatalf("Load (empty dir): %v", err)
	}
}
