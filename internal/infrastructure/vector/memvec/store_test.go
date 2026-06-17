// Package memvec_test exercises the in-memory Store via the ports.VectorStorage contract.
package memvec_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"
)

const (
	testRepo   = "repo1"
	testBranch = "main"
	testModel  = "nomic-embed-text"
)

func makeRow(nodeID string, vec []float32) domain.EmbeddingRow {
	return domain.EmbeddingRow{
		NodeID:      nodeID,
		Vector:      vec,
		ContentHash: "hash-" + nodeID,
		ModelID:     testModel,
	}
}

func vec(vals ...float32) []float32 { return vals }

// TestUpsertAndSearch verifies that calling Search returns previously inserted embedding rows.
func TestUpsertAndSearch(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	rows := []domain.EmbeddingRow{
		makeRow("n1", vec(1, 0, 0)),
		makeRow("n2", vec(0, 1, 0)),
		makeRow("n3", vec(0, 0, 1)),
	}
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	// Query with a vector closest to n1.
	hits, err := s.Search(ctx, testRepo, testBranch, vec(1, 0, 0), 1, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search: got %d hits, want 1", len(hits))
	}
	if hits[0].NodeID != "n1" {
		t.Errorf("Search: top hit NodeID=%q, want %q", hits[0].NodeID, "n1")
	}
}

// TestUpsertReplaces verifies that upserting an existing node identifier replaces the row and updates the vector.
func TestUpsertReplaces(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, []domain.EmbeddingRow{
		makeRow("n1", vec(1, 0, 0)),
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Replace the existing node n1 with a different vector.
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, []domain.EmbeddingRow{
		makeRow("n1", vec(0, 1, 0)),
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Search for the updated vector to confirm that the node now matches the new values.
	hits, err := s.Search(ctx, testRepo, testBranch, vec(0, 1, 0), 1, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != "n1" {
		t.Errorf("Search: got %+v, want [{NodeID:n1 ...}]", hits)
	}
}

// TestSearchEmptyStore verifies that searching an empty store returns a slice of zero length.
func TestSearchEmptyStore(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	hits, err := s.Search(ctx, testRepo, testBranch, vec(1, 0), 5, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("Search on empty store: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Search empty: got %d hits, want 0", len(hits))
	}
}

// TestSearchKLargerThanCorpus verifies that requesting a search limit larger than the total number of rows returns all available rows.
func TestSearchKLargerThanCorpus(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	rows := []domain.EmbeddingRow{
		makeRow("a", vec(1, 0)),
		makeRow("b", vec(0, 1)),
	}
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	hits, err := s.Search(ctx, testRepo, testBranch, vec(1, 0), 100, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("Search k>n: got %d hits, want 2", len(hits))
	}
}

// TestSearchFilterByModel verifies that specifying a model filter restricts search results to only matching rows.
func TestSearchFilterByModel(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	rowA := domain.EmbeddingRow{NodeID: "a", Vector: vec(1, 0), ContentHash: "ha", ModelID: "model-a"}
	rowB := domain.EmbeddingRow{NodeID: "b", Vector: vec(1, 0), ContentHash: "hb", ModelID: "model-b"}
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, []domain.EmbeddingRow{rowA, rowB}); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	hits, err := s.Search(ctx, testRepo, testBranch, vec(1, 0), 10, domain.VectorFilter{ModelID: "model-a"})
	if err != nil {
		t.Fatalf("Search with filter: %v", err)
	}
	for _, h := range hits {
		if h.NodeID != "a" {
			t.Errorf("Search filter: unexpected NodeID %q (want only model-a rows)", h.NodeID)
		}
	}
	if len(hits) != 1 {
		t.Errorf("Search filter: got %d hits, want 1", len(hits))
	}
}

// TestReindexNoOp verifies that Reindex returns a nil error on this backend.
func TestReindexNoOp(t *testing.T) {
	s := memvec.New()
	if err := s.Reindex(context.Background(), testRepo, testModel); err != nil {
		t.Errorf("Reindex: expected nil, got %v", err)
	}
}

// TestLookupContentHashes verifies that LookupContentHashes returns content hashes for existing node identifiers.
func TestLookupContentHashes(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	rows := []domain.EmbeddingRow{
		makeRow("n1", vec(1, 0)),
		makeRow("n2", vec(0, 1)),
	}
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	hashes, err := s.LookupContentHashes(ctx, testRepo, testBranch, []string{"n1", "n2", "n99"})
	if err != nil {
		t.Fatalf("LookupContentHashes: %v", err)
	}
	if hashes["n1"] != "hash-n1" {
		t.Errorf("n1 hash: got %q, want %q", hashes["n1"], "hash-n1")
	}
	if hashes["n2"] != "hash-n2" {
		t.Errorf("n2 hash: got %q, want %q", hashes["n2"], "hash-n2")
	}
	if _, ok := hashes["n99"]; ok {
		t.Errorf("n99 should not be present in result")
	}
}

// TestSearchScoreDescending verifies that search results are ordered by similarity score descending.
func TestSearchScoreDescending(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	rows := []domain.EmbeddingRow{
		makeRow("far", vec(0, 0, 1)),
		makeRow("mid", vec(0.5, 0, 0)),
		makeRow("near", vec(1, 0, 0)),
	}
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	hits, err := s.Search(ctx, testRepo, testBranch, vec(1, 0, 0), 3, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Errorf("hits not sorted descending: hits[%d].Score=%f > hits[%d].Score=%f",
				i, hits[i].Score, i-1, hits[i-1].Score)
		}
	}
	if len(hits) > 0 && hits[0].NodeID != "near" {
		t.Errorf("top hit: got %q, want %q", hits[0].NodeID, "near")
	}
}

// TestCrossRepoBranchIsolation verifies that repository and branch partitions are isolated from each other.
func TestCrossRepoBranchIsolation(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()

	// Insert row data into separate repository partitions.
	if err := s.UpsertEmbeddings(ctx, "repo1", "main", []domain.EmbeddingRow{
		makeRow("r1", vec(1, 0)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEmbeddings(ctx, "repo2", "main", []domain.EmbeddingRow{
		makeRow("r2", vec(1, 0)),
	}); err != nil {
		t.Fatal(err)
	}

	// Query the first repository to ensure that search results do not bleed across partitions.
	hits, err := s.Search(ctx, "repo1", "main", vec(1, 0), 10, domain.VectorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.NodeID != "r1" {
			t.Errorf("repo1 search returned unexpected nodeID %q", h.NodeID)
		}
	}
}
