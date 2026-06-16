// Package bench provides indexed-lookup latency benchmarks for the branchpk SQLite schema.
// It measures warm p50/p95/p99 for the two hot queries (get_node, get_edges) and
// compares them against budgets.
package bench

import (
	"database/sql"
	"fmt"
	"math/rand"
	"slices"
	"time"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/pkloader"
)

const (
	// NodeBudgetMs is the p95 budget for eng_get_node (SELECT * FROM nodes WHERE node_id=? AND branch=?).
	NodeBudgetMs = 25.0
	// EdgesBudgetMs is the p95 budget for eng_get_edges / get_call_chain proxy.
	EdgesBudgetMs = 100.0
)

// LatencyStats holds percentile measurements for a query in milliseconds.
type LatencyStats struct {
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	N     int     `json:"n"`
}

// BenchResult is the complete output record for one benchmark run.
type BenchResult struct {
	OverlapPct    int          `json:"overlap_pct"`
	Branches      int          `json:"branches"`
	Symbols       int          `json:"symbols"`
	NodeLatency   LatencyStats `json:"node_latency"`
	EdgesLatency  LatencyStats `json:"edges_latency"`
	NodeBudgetMs  float64      `json:"node_budget_ms"`
	EdgesBudgetMs float64      `json:"edges_budget_ms"`
	NodePass      bool         `json:"node_pass"`
	EdgesPass     bool         `json:"edges_pass"`
}

// Percentiles computes p50/p95/p99 from an unsorted duration slice.
// The input slice is copied before sorting so the caller's slice is not mutated.
func Percentiles(durs []time.Duration) LatencyStats {
	if len(durs) == 0 {
		return LatencyStats{}
	}

	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	slices.Sort(sorted)

	n := len(sorted)
	return LatencyStats{
		P50Ms: toMs(percentileIdx(sorted, n, 50)),
		P95Ms: toMs(percentileIdx(sorted, n, 95)),
		P99Ms: toMs(percentileIdx(sorted, n, 99)),
		N:     n,
	}
}

// percentileIdx returns the duration at the given percentile (1-100) using
// the nearest-rank method (ceiling index).
func percentileIdx(sorted []time.Duration, n, pct int) time.Duration {
	// ceiling rank: ceil(pct/100 * n), clamped to [1, n]
	rank := max(1, min((pct*n+99)/100, n))
	return sorted[rank-1]
}

func toMs(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

// GetNode runs: SELECT * FROM nodes WHERE node_id=? AND branch=?
// Returns sql.ErrNoRows (wrapped) if the node is not found.
func GetNode(db *sql.DB, nodeID, branch string) error {
	row := db.QueryRow(
		`SELECT node_id, branch, repo_id, language, kind, symbol_path, file_path,
		        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
		 FROM nodes WHERE node_id=? AND branch=?`,
		nodeID, branch,
	)
	var (
		nid, br, repoID, lang, kind, symPath, filePath string
		lineStart, lineEnd                             int
		contentHash, actorID, actorKind                string
		lastPromoted                                   int64
	)
	if err := row.Scan(&nid, &br, &repoID, &lang, &kind, &symPath, &filePath,
		&lineStart, &lineEnd, &contentHash, &lastPromoted, &actorID, &actorKind); err != nil {
		return fmt.Errorf("get_node(%s, %s): %w", nodeID, branch, err)
	}
	return nil
}

// GetEdges runs: SELECT * FROM edges WHERE src_node_id=? AND branch=? AND kind=?
// Returns the number of matching rows.
func GetEdges(db *sql.DB, srcNodeID, branch, kind string) (int, error) {
	rows, err := db.Query(
		`SELECT edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
		 FROM edges WHERE src_node_id=? AND branch=? AND kind=?`,
		srcNodeID, branch, kind,
	)
	if err != nil {
		return 0, fmt.Errorf("get_edges(%s, %s, %s): %w", srcNodeID, branch, kind, err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var edgeID, br, repoID, src, dst, k, conf string
		var lastPromoted int64
		if err := rows.Scan(&edgeID, &br, &repoID, &src, &dst, &k, &conf, &lastPromoted); err != nil {
			return count, fmt.Errorf("scan edge: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("rows error: %w", err)
	}
	return count, nil
}

// RunNodeBench runs n warm queries against random node_ids from symbols on random branches.
// It records individual query latencies and returns aggregated stats.
func RunNodeBench(db *sql.DB, symbols []pkloader.Symbol, branches []string, n int, rng *rand.Rand) (LatencyStats, error) {
	durs := make([]time.Duration, 0, n)

	for range n {
		sym := symbols[rng.Intn(len(symbols))]
		branch := branches[rng.Intn(len(branches))]

		start := time.Now()
		// Ignore ErrNoRows — the symbol may not exist on this branch (overlap scenario).
		_ = GetNode(db, sym.NodeID, branch)
		durs = append(durs, time.Since(start))
	}

	return Percentiles(durs), nil
}

// RunEdgesBench runs n warm queries against random src_node_ids from symbols.
func RunEdgesBench(db *sql.DB, symbols []pkloader.Symbol, branches []string, n int, rng *rand.Rand) (LatencyStats, error) {
	durs := make([]time.Duration, 0, n)

	for range n {
		sym := symbols[rng.Intn(len(symbols))]
		branch := branches[rng.Intn(len(branches))]

		start := time.Now()
		_, _ = GetEdges(db, sym.NodeID, branch, "CALLS")
		durs = append(durs, time.Since(start))
	}

	return Percentiles(durs), nil
}
