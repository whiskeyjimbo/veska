package lancedb_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/hnsw/eval"
	ldb "github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/hnsw/lancedb"
	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/sqlitevec/gen"
)

const smallN = 500 // lancedb is heavier; keep CI test small

func newIndex(t *testing.T) *ldb.Index {
	t.Helper()
	dir := t.TempDir()
	idx, err := ldb.New(filepath.Join(dir, "lance.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func buildIndex(t *testing.T, n int) *ldb.Index {
	t.Helper()
	idx := newIndex(t)
	vecs := gen.GenerateVectors(n, 1)
	for i, v := range vecs {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	return idx
}

// TestLanceDBBasicSearch verifies Add + Search returns k results.
func TestLanceDBBasicSearch(t *testing.T) {
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

// TestLanceDBRecall verifies recall@10 at smallN is positive.
func TestLanceDBRecall(t *testing.T) {
	corpus := gen.GenerateVectors(smallN, 2)
	dir := t.TempDir()
	idx, err := ldb.New(filepath.Join(dir, "lance.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	holdOut := gen.GenerateVectors(10, 9999)
	res := eval.MeasureRecallAndLatency(idx, corpus, holdOut)
	if res.RecallAt10 <= 0 {
		t.Errorf("recall@10 should be positive at %d vectors, got %.4f", smallN, res.RecallAt10)
	}
	t.Logf("lancedb recall@10 at %d: %.4f, p95=%.2fms", smallN, res.RecallAt10, res.P95Ms)
}

// TestLanceDBBackupRoundTrip verifies Save (copy dir) → Load → same top-1 results.
func TestLanceDBBackupRoundTrip(t *testing.T) {
	corpus := gen.GenerateVectors(smallN, 3)
	dir := t.TempDir()
	idx, err := ldb.New(filepath.Join(dir, "lance.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	for i, v := range corpus {
		if err := idx.Add(uint64(i), v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	holdOut := gen.GenerateVectors(5, 7777)
	before := make([][]uint64, len(holdOut))
	for i, q := range holdOut {
		res, err := idx.Search(q, 10)
		if err != nil {
			t.Fatalf("Search before save: %v", err)
		}
		before[i] = res
	}

	// Save = copy directory.
	savedDir := filepath.Join(dir, "backup")
	if err := idx.Save(savedDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate tar round-trip: copy saved directory to another location.
	restoredDir := filepath.Join(dir, "restored")
	if err := copyDir(savedDir, restoredDir); err != nil {
		t.Fatalf("copy dir: %v", err)
	}

	if err := idx.Load(restoredDir); err != nil {
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
	t.Logf("lancedb backup round-trip: PASS (5 hold-out queries)")
}

// copyDir is a simple directory copy helper for tests.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
