package recall_test

import (
	"database/sql"
	"encoding/json"
	"math"
	"testing"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/sqlitevec/recall"
)

func init() {
	vec.Auto()
}

// openMemDB creates an in-memory SQLite DB with the vec_nodes virtual table.
func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open mem db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS vec_nodes USING vec0(embedding FLOAT[768])`); err != nil {
		db.Close()
		t.Fatalf("create vec_nodes: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertVecs inserts a slice of vectors into the DB, returning their assigned rowids (1-indexed).
func insertVecs(t *testing.T, db *sql.DB, vecs [][]float32) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`INSERT INTO vec_nodes(embedding) VALUES (?)`)
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer stmt.Close()
	for i, v := range vecs {
		blob, err := vec.SerializeFloat32(v)
		if err != nil {
			t.Fatalf("serialize vec[%d]: %v", i, err)
		}
		if _, err := stmt.Exec(blob); err != nil {
			t.Fatalf("insert vec[%d]: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// makeVec768 builds a 768-dim vector with the given values repeated.
func makeVec768(vals ...float32) []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = vals[i%len(vals)]
	}
	return v
}

// l2 computes L2 distance between two 768-dim vectors for test assertions.
func l2(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum)
}

// TestBruteForceKNN verifies that BruteForceKNN returns the correct nearest neighbor
// from a small, hand-crafted corpus.
func TestBruteForceKNN(t *testing.T) {
	// 5 corpus vectors (all 768-dim); expected nearest to query is index 2 (rowid 3).
	corpus := [][]float32{
		makeVec768(10.0),
		makeVec768(5.0),
		makeVec768(1.0), // closest to query
		makeVec768(8.0),
		makeVec768(9.0),
	}
	query := makeVec768(1.1) // very close to corpus[2]

	// Sanity: verify distances manually.
	dists := make([]float64, len(corpus))
	for i, c := range corpus {
		dists[i] = l2(c, query)
	}
	// corpus[2] should have the smallest distance.
	minIdx := 0
	for i := 1; i < len(dists); i++ {
		if dists[i] < dists[minIdx] {
			minIdx = i
		}
	}
	if minIdx != 2 {
		t.Fatalf("test setup error: expected corpus[2] closest, got corpus[%d]", minIdx)
	}

	got := recall.BruteForceKNN(corpus, query, 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	// Rowids are 1-indexed: corpus[2] → rowid 3.
	if got[0] != 3 {
		t.Errorf("BruteForceKNN: expected rowid 3, got %d", got[0])
	}

	// Also test k=2; the second nearest should be corpus[1] (value 5.0) — dist to 1.1 is
	// smaller than corpus[3] (8.0), corpus[4] (9.0), corpus[0] (10.0).
	got2 := recall.BruteForceKNN(corpus, query, 2)
	if len(got2) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got2))
	}
	if got2[0] != 3 {
		t.Errorf("BruteForceKNN k=2 first: expected rowid 3, got %d", got2[0])
	}
	if got2[1] != 2 {
		t.Errorf("BruteForceKNN k=2 second: expected rowid 2 (corpus[1]=5.0), got %d", got2[1])
	}
}

// TestRecallPerfect verifies that RunRecall returns recall@10 = 1.0 when vec0 and
// brute-force agree (small corpus where approximation is exact).
func TestRecallPerfect(t *testing.T) {
	db := openMemDB(t)

	const n = 200
	const nHoldOut = 10

	// Use a fixed seed for reproducibility.
	const seed uint64 = 42
	corpus := recall.GenerateHoldOut(n, seed)
	insertVecs(t, db, corpus)

	holdOut := recall.GenerateHoldOut(nHoldOut, seed^0xaaaa)

	res, err := recall.RunRecall(db, corpus, holdOut, int64(n))
	if err != nil {
		t.Fatalf("RunRecall: %v", err)
	}

	if res.HoldOutSize != nHoldOut {
		t.Errorf("HoldOutSize: expected %d, got %d", nHoldOut, res.HoldOutSize)
	}
	if res.Population != int64(n) {
		t.Errorf("Population: expected %d, got %d", n, res.Population)
	}

	// For a small corpus, vec0 is exact — recall@10 should be 1.0.
	// Allow a small tolerance in case vec0 uses approximate search.
	if res.RecallAt10 < 0.9 {
		t.Errorf("RecallAt10: expected >= 0.9 for small corpus, got %.4f", res.RecallAt10)
	}
}

// TestRecallAtKComputation verifies the ComputeRecall function with hand-crafted inputs.
func TestRecallAtKComputation(t *testing.T) {
	tests := []struct {
		name        string
		groundTruth []int64
		returned    []int64
		want        float64
	}{
		{
			name:        "perfect match",
			groundTruth: []int64{1, 2, 3, 4, 5},
			returned:    []int64{1, 2, 3, 4, 5},
			want:        1.0,
		},
		{
			name:        "half match",
			groundTruth: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			returned:    []int64{1, 2, 3, 4, 5, 11, 12, 13, 14, 15},
			want:        0.5,
		},
		{
			name:        "no match",
			groundTruth: []int64{1, 2, 3},
			returned:    []int64{4, 5, 6},
			want:        0.0,
		},
		{
			name:        "empty returned",
			groundTruth: []int64{1, 2, 3},
			returned:    []int64{},
			want:        0.0,
		},
		{
			name:        "single perfect",
			groundTruth: []int64{7},
			returned:    []int64{7},
			want:        1.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := recall.ComputeRecall(tc.groundTruth, tc.returned)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("ComputeRecall(%v, %v) = %.6f, want %.6f", tc.groundTruth, tc.returned, got, tc.want)
			}
		})
	}
}

// TestRecallResultJSON verifies that RecallResult marshals to JSON with the expected fields.
func TestRecallResultJSON(t *testing.T) {
	r := recall.RecallResult{
		Population:  50_000,
		RecallAt10:  0.97,
		RecallAt50:  0.99,
		HoldOutSize: 100,
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	required := []string{"population", "recall_at_10", "recall_at_50", "hold_out_size"}
	for _, field := range required {
		if _, ok := m[field]; !ok {
			t.Errorf("JSON missing field %q", field)
		}
	}

	if got := m["population"]; got != float64(50_000) {
		t.Errorf("population: expected 50000, got %v", got)
	}
	if got := m["recall_at_10"]; got != 0.97 {
		t.Errorf("recall_at_10: expected 0.97, got %v", got)
	}
	if got := m["recall_at_50"]; got != 0.99 {
		t.Errorf("recall_at_50: expected 0.99, got %v", got)
	}
	if got := m["hold_out_size"]; got != float64(100) {
		t.Errorf("hold_out_size: expected 100, got %v", got)
	}
}

// TestHoldOutDeterministic verifies that GenerateHoldOut with the same seed
// always returns the same vectors.
func TestHoldOutDeterministic(t *testing.T) {
	const seed uint64 = 12345
	const n = 20

	first := recall.GenerateHoldOut(n, seed)
	second := recall.GenerateHoldOut(n, seed)

	if len(first) != n || len(second) != n {
		t.Fatalf("expected %d vectors, got %d and %d", n, len(first), len(second))
	}

	for i := range first {
		for j := range first[i] {
			if first[i][j] != second[i][j] {
				t.Errorf("vector[%d][%d]: first=%.6f second=%.6f (not deterministic)", i, j, first[i][j], second[i][j])
			}
		}
	}

	// Also verify different seeds produce different vectors.
	third := recall.GenerateHoldOut(n, seed+1)
	same := true
	for i := range first {
		for j := range first[i] {
			if first[i][j] != third[i][j] {
				same = false
				break
			}
		}
		if !same {
			break
		}
	}
	if same {
		t.Error("different seeds produced identical vectors (seed+1 should differ)")
	}
}
