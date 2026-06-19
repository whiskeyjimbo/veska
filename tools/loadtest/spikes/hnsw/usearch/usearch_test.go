// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package usearch_test

import (
	"os"
	"path/filepath"
	"testing"

	usearchlib "github.com/unum-cloud/usearch/golang"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/hnsw/eval"
	uidx "github.com/whiskeyjimbo/veska/tools/loadtest/spikes/hnsw/usearch"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
)

const smallN = 1000

func buildIndex(t *testing.T, n int, quant usearchlib.Quantization) *uidx.Index {
	t.Helper()
	idx, err := uidx.New(quant)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = idx.Destroy() })
	vecs := gen.GenerateVectors(n, 1)
	for i, v := range vecs {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	return idx
}

// TestUsearchBasicSearch verifies that Add + Search returns k results.
func TestUsearchBasicSearch(t *testing.T) {
	idx := buildIndex(t, smallN, usearchlib.F32)
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

// TestUsearchRecall verifies recall@10 at 1k is positive.
func TestUsearchRecall(t *testing.T) {
	corpus := gen.GenerateVectors(smallN, 2)
	idx, err := uidx.New(usearchlib.F32)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Destroy()
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
	t.Logf("usearch float32 recall@10 at %d: %.4f, p95=%.2fms", smallN, res.RecallAt10, res.P95Ms)
}

// TestUsearchBackupRoundTrip verifies Save → Load → Search returns same results.
func TestUsearchBackupRoundTrip(t *testing.T) {
	corpus := gen.GenerateVectors(smallN, 3)
	idx, err := uidx.New(usearchlib.F32)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Destroy()
	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "usearch.bin")

	// 5 hold-out queries for backup round-trip correctness.
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

	// Simulate tar round-trip: copy file to another path.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	restored := filepath.Join(dir, "usearch_restored.bin")
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
		// All top-1 results should match.
		if len(before[i]) == 0 || len(after) == 0 {
			continue
		}
		if before[i][0] != after[0] {
			t.Errorf("query %d: top-1 before=%d after=%d (round-trip mismatch)", i, before[i][0], after[0])
		}
	}
	t.Logf("usearch backup round-trip: PASS (5 hold-out queries)")
}
