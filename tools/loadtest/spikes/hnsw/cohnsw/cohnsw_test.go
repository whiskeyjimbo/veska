// SPDX-License-Identifier: AGPL-3.0-only

package cohnsw_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/hnsw/cohnsw"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/hnsw/eval"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
)

const smallN = 1000

func buildIndex(t *testing.T, n int) *cohnsw.Index {
	t.Helper()
	idx := cohnsw.New()
	vecs := gen.GenerateVectors(n, 1)
	for i, v := range vecs {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	return idx
}

// TestCohNSWBasicSearch verifies Add + Search returns k results.
func TestCohNSWBasicSearch(t *testing.T) {
	idx := buildIndex(t, smallN)
	query := gen.GenerateVectors(1, 42)[0]
	results, err := idx.Search(query, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 10 {
		t.Errorf("expected 10 results, got %d", len(results))
	}
	if idx.Len() != smallN {
		t.Errorf("Len: got %d, want %d", idx.Len(), smallN)
	}
}

// TestCohNSWRecall verifies recall@10 at 1k is positive.
func TestCohNSWRecall(t *testing.T) {
	corpus := gen.GenerateVectors(smallN, 2)
	idx := cohnsw.New()
	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	holdOut := gen.GenerateVectors(20, 9999)
	res := eval.MeasureRecallAndLatency(idx, corpus, holdOut)
	if res.RecallAt10 <= 0 {
		t.Errorf("recall@10 should be positive at 1k vectors, got %.4f", res.RecallAt10)
	}
	t.Logf("coder/hnsw recall@10 at %d: %.4f, p95=%.2fms", smallN, res.RecallAt10, res.P95Ms)
}

// TestCohNSWBackupRoundTrip verifies Save → Load → Search returns same top-1 results.
func TestCohNSWBackupRoundTrip(t *testing.T) {
	corpus := gen.GenerateVectors(smallN, 3)
	idx := cohnsw.New()
	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "cohnsw.bin")

	holdOut := gen.GenerateVectors(5, 7777)
	before := make([][]uint64, len(holdOut))
	for i, q := range holdOut {
		res, err := idx.Search(q, 10)
		if err != nil {
			t.Fatalf("Search before save: %v", err)
		}
		before[i] = res
	}

	if err := idx.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate tar round-trip: copy to a new path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	restored := filepath.Join(dir, "cohnsw_restored.bin")
	if err := os.WriteFile(restored, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := idx.Load(restored); err != nil {
		t.Fatalf("Load: %v", err)
	}

	for i, q := range holdOut {
		after, err := idx.Search(q, 10)
		if err != nil {
			t.Fatalf("Search after load: %v", err)
		}
		if len(before[i]) == 0 || len(after) == 0 {
			continue
		}
		if before[i][0] != after[0] {
			t.Errorf("query %d: top-1 before=%d after=%d (round-trip mismatch)", i, before[i][0], after[0])
		}
	}
	t.Logf("coder/hnsw backup round-trip: PASS (5 hold-out queries)")
}
