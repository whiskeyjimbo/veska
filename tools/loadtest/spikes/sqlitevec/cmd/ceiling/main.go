// Command ceiling sweeps vec0 query latency across user-specified populations to find
// the node count at which warm p95 first exceeds the SOLO-13 §3.1 budget (100ms).
// Each population is loaded into a fresh temporary DB so measurements are independent.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/bench"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
)

func init() { vec.Auto() }

type popResult struct {
	Population  int64              `json:"population"`
	Warm        bench.LatencyStats `json:"warm"`
	RSSBytes    int64              `json:"rss_bytes"`
	PassLatency bool               `json:"pass_latency"`
	PassRSS     bool               `json:"pass_rss"`
}

type ceilingReport struct {
	Populations      []popResult `json:"populations"`
	Vec0Ceiling      int64       `json:"vec0_ceiling"`
	CeilingReason    string      `json:"ceiling_reason"`
	MmapBytes        int64       `json:"mmap_bytes"`
	CacheSizeKB      int64       `json:"cache_size_kb"`
	PageSize         int64       `json:"page_size"`
	SqliteVecVersion string      `json:"sqlite_vec_version"`
	SqliteVersion    string      `json:"sqlite_version"`
	Platform         string      `json:"platform"`
}

func main() {
	popsFlag := flag.String("populations", "50000,100000,200000,400000,800000",
		"comma-separated node counts to sweep")
	queries := flag.Int("queries", 200, "warm queries per population")
	outFlag := flag.String("out", "data/ceiling_metrics.json", "output JSON path")
	tmpDir := flag.String("tmpdir", "", "directory for temp DBs (default: system temp)")
	seed := flag.Uint64("seed", 42, "RNG seed")
	// mmap maps the DB file into virtual memory — major speedup on M1/M2 unified memory.
	// Default covers 2M vectors × 768 dims × 4 bytes ≈ 6 GiB with headroom.
	mmapBytes := flag.Int64("mmap", 0, "mmap_size in bytes (0 to disable)")
	cacheSizeKB := flag.Int64("cache", 0, "cache_size in KiB (0 = SQLite default)")
	pageSize := flag.Int("pagesize", 0, "page_size in bytes, power-of-two in [512,65536] (0 = SQLite default 4096)")
	flag.Parse()

	pops, err := parsePops(*popsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: -populations: %v\n", err)
		os.Exit(1)
	}

	rng := rand.New(rand.NewPCG(*seed, 0))

	var results []popResult
	var ceiling int64
	var ceilingReason = "none"

	// Versions from a scratch DB.
	vecVer, sqliteVer, err := versionsFromScratch(*tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: versions: %v\n", err)
		os.Exit(1)
	}

	for _, pop := range pops {
		fmt.Fprintf(os.Stderr, "→ population %d: loading vectors…\n", pop)

		dbPath, cleanup, err := makeTempDB(*tmpDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: make temp db: %v\n", err)
			os.Exit(1)
		}

		if err := loadVectors(dbPath, int(pop), *pageSize, rng); err != nil {
			cleanup()
			fmt.Fprintf(os.Stderr, "error: load vectors at %d: %v\n", pop, err)
			os.Exit(1)
		}

		db, err := openBenchDB(dbPath, *mmapBytes, *cacheSizeKB)
		if err != nil {
			cleanup()
			fmt.Fprintf(os.Stderr, "error: open bench db at %d: %v\n", pop, err)
			os.Exit(1)
		}

		start := time.Now()
		warm, err := bench.RunQueryBench(db, 10, *queries, rng)
		db.Close()
		cleanup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: bench at %d: %v\n", pop, err)
			os.Exit(1)
		}

		rss := loader.ReadRSSBytes()
		passLat := warm.P95Ms <= bench.BudgetLatencyMs
		passRSS := rss <= bench.BudgetRSSBytes

		fmt.Fprintf(os.Stderr, "  p95=%.2fms  rss=%.1fMiB  elapsed=%s  %s\n",
			warm.P95Ms,
			float64(rss)/float64(1<<20),
			time.Since(start).Round(time.Millisecond),
			verdict(passLat, passRSS),
		)

		results = append(results, popResult{
			Population:  pop,
			Warm:        warm,
			RSSBytes:    rss,
			PassLatency: passLat,
			PassRSS:     passRSS,
		})

		if ceiling == 0 {
			if !passLat {
				ceiling = pop
				ceilingReason = "latency"
			} else if !passRSS {
				ceiling = pop
				ceilingReason = "rss"
			}
		}
	}

	report := ceilingReport{
		Populations:      results,
		Vec0Ceiling:      ceiling,
		CeilingReason:    ceilingReason,
		MmapBytes:        *mmapBytes,
		CacheSizeKB:      *cacheSizeKB,
		SqliteVecVersion: vecVer,
		SqliteVersion:    sqliteVer,
		Platform:         bench.PlatformString(),
		PageSize:         int64(*pageSize),
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal: %v\n", err)
		os.Exit(1)
	}

	outPath, _ := filepath.Abs(*outFlag)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write output: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "%s\n", data)

	if ceiling > 0 {
		fmt.Fprintf(os.Stderr, "\nvec0 ceiling: %d nodes (%s)\n", ceiling, ceilingReason)
	} else {
		fmt.Fprintf(os.Stderr, "\nvec0 ceiling: not reached across tested populations\n")
	}
	fmt.Fprintf(os.Stderr, "Written to %s\n", outPath)
}

func openBenchDB(path string, mmapBytes, cacheSizeKB int64) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	pragmas := []string{
		fmt.Sprintf(`PRAGMA mmap_size = %d`, mmapBytes),
		fmt.Sprintf(`PRAGMA cache_size = -%d`, cacheSizeKB), // negative = KiB units
		`PRAGMA temp_store = MEMORY`,
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return db, nil
}

func parsePops(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid population %q", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one population required")
	}
	return out, nil
}

func makeTempDB(dir string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp(dir, "ceiling-*.db")
	if err != nil {
		return "", nil, err
	}
	f.Close()
	path = f.Name()
	return path, func() {
		os.Remove(path)
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
	}, nil
}

func loadVectors(dbPath string, n, pageSize int, rng *rand.Rand) error {
	l, err := loader.OpenWithPageSize(dbPath, pageSize)
	if err != nil {
		return err
	}
	defer l.Close()

	vecs := generateParallel(n, rng.Uint64())
	const batchSize = 10_000
	for i := 0; i < len(vecs); i += batchSize {
		end := min(i+batchSize, len(vecs))
		if err := l.InsertBatch(vecs[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// generateParallel generates n vectors by splitting work across NumCPU goroutines.
// Each goroutine gets a distinct seed derived from the base seed so results are
// deterministic but independent.
func generateParallel(n int, baseSeed uint64) [][]float32 {
	workers := runtime.NumCPU()
	vecs := make([][]float32, n)

	chunkSize := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := range workers {
		start := w * chunkSize
		end := min(start+chunkSize, n)
		if start >= n {
			break
		}
		wg.Add(1)
		go func(start, end int, seed uint64) {
			defer wg.Done()
			chunk := gen.GenerateVectors(end-start, seed)
			copy(vecs[start:end], chunk)
		}(start, end, baseSeed^uint64(w)*0x9e3779b97f4a7c15)
	}
	wg.Wait()
	return vecs
}

func versionsFromScratch(dir string) (vecVer, sqliteVer string, err error) {
	f, err := os.CreateTemp(dir, "ver-*.db")
	if err != nil {
		return "", "", err
	}
	f.Close()
	defer os.Remove(f.Name())

	db, err := sql.Open("sqlite3", f.Name())
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	return bench.Versions(db)
}

func verdict(passLat, passRSS bool) string {
	if passLat && passRSS {
		return "PASS"
	}
	if !passLat {
		return "FAIL (latency)"
	}
	return "FAIL (rss)"
}
