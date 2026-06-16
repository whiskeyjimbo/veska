package bench_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/bench"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/pkloader"
)

// openMemDB creates a fresh in-memory SQLite DB with the branchpk schema and
// inserts 500 symbols on a single branch, returning the DB and the symbols.
func openMemDB(t *testing.T) (*sql.DB, []pkloader.Symbol, []string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := pkloader.CreateSchema(db); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := pkloader.InsertRepo(db, "testrepo"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	const branch = "main"
	syms := pkloader.GenerateBaseSymbols(500, "testrepo")
	if err := pkloader.InsertBranch(db, branch, "testrepo", syms, 1700000000); err != nil {
		t.Fatalf("insert branch: %v", err)
	}

	return db, syms, []string{branch}
}

// TestPercentiles verifies that p50/p95/p99 are computed correctly for a known input.
func TestPercentiles(t *testing.T) {
	// 100 durations: 1ms … 100ms
	durs := make([]time.Duration, 100)
	for i := range durs {
		durs[i] = time.Duration(i+1) * time.Millisecond
	}

	stats := bench.Percentiles(durs)

	if stats.N != 100 {
		t.Errorf("N: got %d want 100", stats.N)
	}

	// p50 of [1.100] = 50th percentile → 50ms (index 49 after sort)
	if stats.P50Ms < 49 || stats.P50Ms > 51 {
		t.Errorf("P50Ms: got %.2f, expected ~50", stats.P50Ms)
	}
	// p95 → around 95ms
	if stats.P95Ms < 94 || stats.P95Ms > 96 {
		t.Errorf("P95Ms: got %.2f, expected ~95", stats.P95Ms)
	}
	// p99 → around 99ms
	if stats.P99Ms < 98 || stats.P99Ms > 100 {
		t.Errorf("P99Ms: got %.2f, expected ~99", stats.P99Ms)
	}
}

// TestGetNodeQuery verifies that GetNode returns a row for a known node_id+branch.
func TestGetNodeQuery(t *testing.T) {
	db, syms, _ := openMemDB(t)

	if err := bench.GetNode(db, syms[0].NodeID, "main"); err != nil {
		t.Fatalf("GetNode: %v", err)
	}

	// Non-existent node should return an error (sql.ErrNoRows wrapped).
	if err := bench.GetNode(db, "does-not-exist", "main"); err == nil {
		t.Fatal("expected error for missing node, got nil")
	}
}

// TestGetEdgesQuery verifies that GetEdges returns at least 1 row for a known src+branch+kind.
func TestGetEdgesQuery(t *testing.T) {
	db, syms, _ := openMemDB(t)

	count, err := bench.GetEdges(db, syms[0].NodeID, "main", "CALLS")
	if err != nil {
		t.Fatalf("GetEdges: %v", err)
	}
	if count < 1 {
		t.Fatalf("GetEdges: got %d rows, want >= 1", count)
	}
}

// TestBenchResultJSON verifies that BenchResult marshals with all required JSON fields.
func TestBenchResultJSON(t *testing.T) {
	result := bench.BenchResult{
		OverlapPct: 10,
		Branches:   1,
		Symbols:    500,
		NodeLatency: bench.LatencyStats{
			P50Ms: 1.0,
			P95Ms: 2.0,
			P99Ms: 3.0,
			N:     200,
		},
		EdgesLatency: bench.LatencyStats{
			P50Ms: 4.0,
			P95Ms: 5.0,
			P99Ms: 6.0,
			N:     200,
		},
		NodeBudgetMs:  bench.NodeBudgetMs,
		EdgesBudgetMs: bench.EdgesBudgetMs,
		NodePass:      true,
		EdgesPass:     true,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{
		"node_budget_ms", "edges_budget_ms",
		"node_pass", "edges_pass",
		"node_latency", "edges_latency",
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("missing JSON field: %s", key)
		}
	}

	// Verify nested latency fields.
	nodeLatency, ok := m["node_latency"].(map[string]any)
	if !ok {
		t.Fatal("node_latency is not an object")
	}
	for _, sub := range []string{"p50_ms", "p95_ms", "p99_ms", "n"} {
		if _, ok := nodeLatency[sub]; !ok {
			t.Errorf("missing node_latency.%s", sub)
		}
	}

	edgesLatency, ok := m["edges_latency"].(map[string]any)
	if !ok {
		t.Fatal("edges_latency is not an object")
	}
	for _, sub := range []string{"p50_ms", "p95_ms", "p99_ms", "n"} {
		if _, ok := edgesLatency[sub]; !ok {
			t.Errorf("missing edges_latency.%s", sub)
		}
	}
}

// TestBudgetGates verifies that pass/fail logic uses the correct budget thresholds.
func TestBudgetGates(t *testing.T) {
	// node_p95 = 20ms → node_pass = true (under 25ms budget)
	passResult := bench.BenchResult{
		NodeLatency:  bench.LatencyStats{P95Ms: 20.0},
		EdgesLatency: bench.LatencyStats{P95Ms: 50.0},
	}
	passResult.NodePass = passResult.NodeLatency.P95Ms < bench.NodeBudgetMs
	passResult.EdgesPass = passResult.EdgesLatency.P95Ms < bench.EdgesBudgetMs

	if !passResult.NodePass {
		t.Errorf("node_p95=20ms should pass 25ms budget, got NodePass=false")
	}
	if !passResult.EdgesPass {
		t.Errorf("edges_p95=50ms should pass 100ms budget, got EdgesPass=false")
	}

	// node_p95 = 30ms → node_pass = false (over 25ms budget)
	failResult := bench.BenchResult{
		NodeLatency:  bench.LatencyStats{P95Ms: 30.0},
		EdgesLatency: bench.LatencyStats{P95Ms: 110.0},
	}
	failResult.NodePass = failResult.NodeLatency.P95Ms < bench.NodeBudgetMs
	failResult.EdgesPass = failResult.EdgesLatency.P95Ms < bench.EdgesBudgetMs

	if failResult.NodePass {
		t.Errorf("node_p95=30ms should fail 25ms budget, got NodePass=true")
	}
	if failResult.EdgesPass {
		t.Errorf("edges_p95=110ms should fail 100ms budget, got EdgesPass=true")
	}
}
