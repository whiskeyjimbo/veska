// SPDX-License-Identifier: AGPL-3.0-only

package bench_test

import (
	"database/sql"
	"encoding/json"
	"math/rand/v2"
	"testing"
	"time"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/bench"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
)

func init() {
	vec.Auto()
}

// openMemDB opens an in-memory SQLite DB with the vec_nodes virtual table.
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
	return db
}

// insertVecs inserts n synthetic vectors into db.
func insertVecs(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	vecs := gen.GenerateVectors(n, 42)
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

// TestPercentiles verifies p50/p95/p99/max on a known slice of durations.
func TestPercentiles(t *testing.T) {
	// 100 durations: 1ms, 2ms,., 100ms
	durs := make([]time.Duration, 100)
	for i := range durs {
		durs[i] = time.Duration(i+1) * time.Millisecond
	}

	stats := bench.Percentiles(durs)

	// p50 -> index 49 of sorted = 50ms
	if got := stats.P50Ms; got < 49.0 || got > 51.0 {
		t.Errorf("p50 = %.2fms, want ~50ms", got)
	}
	// p95 -> index 94 of sorted = 95ms
	if got := stats.P95Ms; got < 94.0 || got > 96.0 {
		t.Errorf("p95 = %.2fms, want ~95ms", got)
	}
	// p99 -> index 98 of sorted = 99ms
	if got := stats.P99Ms; got < 98.0 || got > 100.0 {
		t.Errorf("p99 = %.2fms, want ~99ms", got)
	}
	// max = 100ms
	if got := stats.MaxMs; got < 99.0 || got > 101.0 {
		t.Errorf("max = %.2fms, want ~100ms", got)
	}
	if stats.N != 100 {
		t.Errorf("N = %d, want 100", stats.N)
	}
}

// TestQueryResultCount verifies a k=10 query returns exactly 10 results with 200 vectors.
func TestQueryResultCount(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()

	insertVecs(t, db, 200)

	queryVec := gen.GenerateVectors(1, 99)[0]
	results, err := bench.QueryVec0(db, queryVec, 10)
	if err != nil {
		t.Fatalf("QueryVec0: %v", err)
	}
	if len(results) != 10 {
		t.Errorf("got %d results, want 10", len(results))
	}
}

// TestQueryResultCountK50 verifies k=50 with only 100 vectors returns min(50, count) results.
func TestQueryResultCountK50(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()

	insertVecs(t, db, 100)

	queryVec := gen.GenerateVectors(1, 77)[0]
	results, err := bench.QueryVec0(db, queryVec, 50)
	if err != nil {
		t.Fatalf("QueryVec0: %v", err)
	}
	want := 50 // 100 vectors, k=50, so 50 results
	if len(results) != want {
		t.Errorf("got %d results, want %d", len(results), want)
	}
}

// TestBenchResultJSON verifies BenchResult marshals to JSON with all required fields.
func TestBenchResultJSON(t *testing.T) {
	br := bench.BenchResult{
		Pops: []bench.PopBench{
			{
				Population: 50000,
				K:          10,
				Warm:       bench.LatencyStats{P50Ms: 1.1, P95Ms: 2.2, P99Ms: 3.3, MaxMs: 4.4, N: 100},
				Cold:       bench.LatencyStats{P50Ms: 5.5, P95Ms: 6.6, P99Ms: 7.7, MaxMs: 8.8, N: 100},
			},
		},
		Vec0Ceiling:      100000,
		CeilingReason:    "latency",
		SqliteVecVersion: "v0.1.6",
		SqliteVersion:    "3.45.0",
		Platform:         "linux/amd64",
	}

	data, err := json.Marshal(br)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	requiredKeys := []string{"populations", "vec0_ceiling", "ceiling_reason", "sqlite_vec_version", "sqlite_version", "platform"}
	for _, k := range requiredKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key: %q", k)
		}
	}

	// Check nested latency fields exist.
	pops, ok := m["populations"].([]any)
	if !ok || len(pops) == 0 {
		t.Fatal("populations missing or empty")
	}
	pop0 := pops[0].(map[string]any)
	for _, k := range []string{"population", "k", "warm", "cold"} {
		if _, ok := pop0[k]; !ok {
			t.Errorf("pop missing JSON key: %q", k)
		}
	}
	warm := pop0["warm"].(map[string]any)
	for _, k := range []string{"p50_ms", "p95_ms", "p99_ms", "max_ms", "n"} {
		if _, ok := warm[k]; !ok {
			t.Errorf("warm missing JSON key: %q", k)
		}
	}
}

// TestCeilingSweepStopsAtBudget verifies the sweep stops when p95 exceeds 100ms.
func TestCeilingSweepStopsAtBudget(t *testing.T) {
	// Mock: p95 = 110ms at population 100k (exceeds 100ms budget).
	// The sweep should identify 100k as the ceiling population.
	populations := []int64{50_000, 100_000, 200_000}
	mockP95 := map[int64]float64{
		50_000:  50.0,  // within budget
		100_000: 110.0, // exceeds budget -> ceiling
		200_000: 200.0, // would be even worse
	}

	ceiling, reason := bench.FindCeilingFromMock(populations, mockP95, nil, bench.BudgetLatencyMs, bench.BudgetRSSBytes)
	if ceiling != 100_000 {
		t.Errorf("ceiling = %d, want 100000", ceiling)
	}
	if reason != "latency" {
		t.Errorf("reason = %q, want \"latency\"", reason)
	}
}

// TestRunQueryBench verifies RunQueryBench returns stats with N = nQueries.
func TestRunQueryBench(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()

	insertVecs(t, db, 200)

	rng := rand.New(rand.NewPCG(1, 2))
	stats, err := bench.RunQueryBench(db, 10, 20, rng)
	if err != nil {
		t.Fatalf("RunQueryBench: %v", err)
	}
	if stats.N != 20 {
		t.Errorf("N = %d, want 20", stats.N)
	}
	if stats.P50Ms < 0 || stats.P95Ms < 0 || stats.P99Ms < 0 || stats.MaxMs < 0 {
		t.Error("negative latency values")
	}
	if stats.MaxMs < stats.P99Ms {
		t.Errorf("max (%.3f) < p99 (%.3f)", stats.MaxMs, stats.P99Ms)
	}
	if stats.P99Ms < stats.P95Ms {
		t.Errorf("p99 (%.3f) < p95 (%.3f)", stats.P99Ms, stats.P95Ms)
	}
}

// TestVersions verifies Versions returns non-empty version strings.
func TestVersions(t *testing.T) {
	db := openMemDB(t)
	defer db.Close()

	vecVer, sqliteVer, err := bench.Versions(db)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if vecVer == "" {
		t.Error("sqlite_vec version empty")
	}
	if sqliteVer == "" {
		t.Error("sqlite version empty")
	}
}

// TestLoaderInsertAndQuery is an integration test using the loader package directly.
func TestLoaderInsertAndQuery(t *testing.T) {
	l, err := loader.Open(":memory:")
	if err != nil {
		t.Fatalf("loader.Open: %v", err)
	}
	defer l.Close()

	vecs := gen.GenerateVectors(50, 11)
	if err := l.InsertBatch(vecs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	count, err := l.RowCount()
	if err != nil {
		t.Fatalf("RowCount: %v", err)
	}
	if count != 50 {
		t.Errorf("RowCount = %d, want 50", count)
	}
}
