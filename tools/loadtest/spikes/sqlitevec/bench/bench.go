// Package bench provides latency benchmarking utilities for sqlite-vec (vec0) KNN queries.
// It measures warm and cold query latency percentiles and sweeps populations to find
// the vec0 ceiling based on latency or RSS budget constraints.
package bench

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"time"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
)

func init() {
	vec.Auto()
}

const (
	// BudgetLatencyMs is the p95 latency budget in milliseconds (k=10).
	BudgetLatencyMs = 100.0
	// BudgetRSSBytes is the soft RSS cap in bytes (2 GiB).
	BudgetRSSBytes = 2 * 1024 * 1024 * 1024
)

// LatencyStats holds percentile latency measurements.
type LatencyStats struct {
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
	N     int     `json:"n"`
}

// PopBench holds bench results for one population at one k.
type PopBench struct {
	Population int64        `json:"population"`
	K          int          `json:"k"`
	Warm       LatencyStats `json:"warm"`
	Cold       LatencyStats `json:"cold"`
}

// BenchResult is the top-level output.
type BenchResult struct {
	Pops             []PopBench `json:"populations"`
	Vec0Ceiling      int64      `json:"vec0_ceiling"`   // node count where budget was exceeded; 0 if not found
	CeilingReason    string     `json:"ceiling_reason"` // "latency" | "rss" | "none"
	SqliteVecVersion string     `json:"sqlite_vec_version"`
	SqliteVersion    string     `json:"sqlite_version"`
	Platform         string     `json:"platform"`
}

// Percentiles computes latency stats from a slice of time.Duration values.
// The input slice is sorted in place.
func Percentiles(durs []time.Duration) LatencyStats {
	n := len(durs)
	if n == 0 {
		return LatencyStats{}
	}

	sorted := make([]time.Duration, n)
	copy(sorted, durs)
	slices.Sort(sorted)

	toMs := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

	p := func(pct float64) float64 {
		idx := int(float64(n-1) * pct)
		return toMs(sorted[idx])
	}

	return LatencyStats{
		P50Ms: p(0.50),
		P95Ms: p(0.95),
		P99Ms: p(0.99),
		MaxMs: toMs(sorted[n-1]),
		N:     n,
	}
}

// QueryVec0 runs a single KNN query against vec_nodes and returns up to k rowids.
// The query vector is serialized as a float32 blob for the sqlite-vec MATCH operator.
func QueryVec0(db *sql.DB, queryVec []float32, k int) ([]int64, error) {
	blob, err := vec.SerializeFloat32(queryVec)
	if err != nil {
		return nil, fmt.Errorf("bench: serialize query vec: %w", err)
	}

	rows, err := db.Query(
		`SELECT rowid FROM vec_nodes WHERE embedding MATCH ? ORDER BY distance LIMIT ?`,
		blob, k,
	)
	if err != nil {
		return nil, fmt.Errorf("bench: knn query: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("bench: scan rowid: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bench: rows error: %w", err)
	}
	return ids, nil
}

// RunQueryBench runs nQueries warm queries at k against the open DB,
// using a random 768-dim query vector each time.
// Returns LatencyStats for the warm pass.
// For cold benchmarking: the caller should close the DB, issue `PRAGMA cache_size=0`
// on reopen, then call RunQueryBench again. This approximates cold-cache behaviour;
// true post-restart cold is not achievable without a process restart.
func RunQueryBench(db *sql.DB, k, nQueries int, rng *rand.Rand) (LatencyStats, error) {
	// Pre-generate query vectors to avoid generation overhead in the timing loop.
	queryVecs := gen.GenerateVectors(nQueries, rng.Uint64())

	durs := make([]time.Duration, 0, nQueries)
	for _, qv := range queryVecs {
		start := time.Now()
		ids, err := QueryVec0(db, qv, k)
		elapsed := time.Since(start)
		if err != nil {
			return LatencyStats{}, fmt.Errorf("bench: query error: %w", err)
		}
		_ = ids
		durs = append(durs, elapsed)
	}
	return Percentiles(durs), nil
}

// Versions returns the sqlite_vec extension version and SQLite version strings.
func Versions(db *sql.DB) (vecVer, sqliteVer string, err error) {
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&vecVer); err != nil {
		return "", "", fmt.Errorf("bench: vec_version: %w", err)
	}
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&sqliteVer); err != nil {
		return "", "", fmt.Errorf("bench: sqlite_version: %w", err)
	}
	return vecVer, sqliteVer, nil
}

// PlatformString returns a "GOOS/GOARCH" platform identifier.
func PlatformString() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// FindCeilingFromMock is the testable core of the ceiling-sweep logic.
// It accepts pre-computed p95 latencies (ms) and RSS bytes per population.
// rssMap may be nil (RSS check skipped when nil).
// Returns the first population that violates either budget, and the reason ("latency"|"rss"|"none").
func FindCeilingFromMock(
	populations []int64,
	p95Map map[int64]float64,
	rssMap map[int64]int64,
	latencyBudgetMs float64,
	rssBudgetBytes int64,
) (ceiling int64, reason string) {
	for _, pop := range populations {
		if p95 := p95Map[pop]; p95 > latencyBudgetMs {
			return pop, "latency"
		}
		if rssMap != nil {
			if rss := rssMap[pop]; rss > rssBudgetBytes {
				return pop, "rss"
			}
		}
	}
	return 0, "none"
}

// RunCeilingSweep loads vectors into a temp DB at each sweep population,
// runs nQueries warm queries at k=10, measures p95 and RSS,
// and returns the ceiling population and reason.
// Sweep populations: 50k, 100k, 200k, 400k, 800k, 1.6M (doubling from 50k up to 2M).
func RunCeilingSweep(dbPath string, nQueries int, rng *rand.Rand) (ceiling int64, reason string, err error) {
	populations := []int64{50_000, 100_000, 200_000, 400_000, 800_000, 1_600_000}

	p95Map := make(map[int64]float64, len(populations))
	rssMap := make(map[int64]int64, len(populations))

	for _, pop := range populations {
		l, err := loader.Open(dbPath)
		if err != nil {
			return 0, "", fmt.Errorf("bench: ceiling sweep open db at pop %d: %w", pop, err)
		}

		vecs := gen.GenerateVectors(int(pop), rng.Uint64())
		batchSize := 10_000
		for i := 0; i < len(vecs); i += batchSize {
			end := min(i+batchSize, len(vecs))
			if err := l.InsertBatch(vecs[i:end]); err != nil {
				l.Close()
				return 0, "", fmt.Errorf("bench: ceiling sweep insert at pop %d: %w", pop, err)
			}
		}

		// We need the underlying *sql.DB for bench queries; re-open via sql.Open.
		// Close the loader and open directly (loader.Open doesn't expose DB).
		l.Close()

		db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
		if err != nil {
			return 0, "", fmt.Errorf("bench: ceiling sweep open raw db at pop %d: %w", pop, err)
		}
		db.SetMaxOpenConns(1)

		stats, err := RunQueryBench(db, 10, nQueries, rng)
		db.Close()
		if err != nil {
			return 0, "", fmt.Errorf("bench: ceiling sweep query at pop %d: %w", pop, err)
		}

		p95Map[pop] = stats.P95Ms
		rssMap[pop] = loader.ReadRSSBytes()

		// Check early stop.
		if stats.P95Ms > BudgetLatencyMs {
			return pop, "latency", nil
		}
		if rssMap[pop] > BudgetRSSBytes {
			return pop, "rss", nil
		}
	}

	return 0, "none", nil
}
