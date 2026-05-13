// Command bench measures warm and cold KNN query latency for sqlite-vec (vec0)
// at multiple populations and k values, then sweeps to find the vec0 ceiling.
//
// Usage:
//
//	bench [-n50k N] [-n1m N] [-queries N] [-out path] [-db path]
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/bench"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
)

func init() {
	vec.Auto()
}

func main() {
	n50k := flag.Int("n50k", 50_000, "population for small bench")
	n1m := flag.Int("n1m", 1_000_000, "population for large bench")
	nQueries := flag.Int("queries", 100, "warm queries per pass")
	outPath := flag.String("out", "data/bench_metrics.json", "output JSON path")
	dbPath := flag.String("db", "data/bench.db", "sqlite db path")
	flag.Parse()

	rng := rand.New(rand.NewPCG(42, 0xbeef))

	var result bench.BenchResult
	result.Platform = bench.PlatformString()

	// Get version info from a temporary in-memory DB.
	versionDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		log.Fatalf("open version db: %v", err)
	}
	versionDB.SetMaxOpenConns(1)
	vecVer, sqliteVer, err := bench.Versions(versionDB)
	versionDB.Close()
	if err != nil {
		log.Fatalf("versions: %v", err)
	}
	result.SqliteVecVersion = vecVer
	result.SqliteVersion = sqliteVer

	populations := []int64{int64(*n50k), int64(*n1m)}
	ks := []int{10, 50}

	for _, pop := range populations {
		for _, k := range ks {
			log.Printf("benching population=%d k=%d ...", pop, k)

			dbFile := filepath.Join(filepath.Dir(*dbPath),
				fmt.Sprintf("bench_%d_k%d.db", pop, k))

			// Load vectors.
			if err := os.Remove(dbFile); err != nil && !os.IsNotExist(err) {
				log.Fatalf("remove old db: %v", err)
			}
			l, err := loader.Open(dbFile)
			if err != nil {
				log.Fatalf("loader.Open: %v", err)
			}

			vecs := gen.GenerateVectors(int(pop), rng.Uint64())
			const batchSize = 10_000
			for i := 0; i < len(vecs); i += batchSize {
				end := min(i+batchSize, len(vecs))
				if err := l.InsertBatch(vecs[i:end]); err != nil {
					l.Close()
					log.Fatalf("InsertBatch: %v", err)
				}
			}
			l.Close()

			// Warm pass.
			warmDB, err := sql.Open("sqlite3", dbFile+"?_journal_mode=WAL")
			if err != nil {
				log.Fatalf("open warm db: %v", err)
			}
			warmDB.SetMaxOpenConns(1)
			warmStats, err := bench.RunQueryBench(warmDB, k, *nQueries, rng)
			warmDB.Close()
			if err != nil {
				log.Fatalf("warm bench: %v", err)
			}

			// Cold pass: reopen with cache_size=0 to evict SQLite page cache.
			// NOTE: this approximates cold-cache behaviour. True post-restart cold
			// requires a process restart to drop OS page cache (drop_caches). This
			// is a known limitation — results reflect SQLite-cache-cold, not OS-cold.
			coldDB, err := sql.Open("sqlite3", dbFile+"?_journal_mode=WAL&cache=shared")
			if err != nil {
				log.Fatalf("open cold db: %v", err)
			}
			coldDB.SetMaxOpenConns(1)
			if _, err := coldDB.Exec(`PRAGMA cache_size=0`); err != nil {
				coldDB.Close()
				log.Fatalf("PRAGMA cache_size=0: %v", err)
			}
			coldStats, err := bench.RunQueryBench(coldDB, k, *nQueries, rng)
			coldDB.Close()
			if err != nil {
				log.Fatalf("cold bench: %v", err)
			}

			result.Pops = append(result.Pops, bench.PopBench{
				Population: pop,
				K:          k,
				Warm:       warmStats,
				Cold:       coldStats,
			})

			log.Printf("  warm p95=%.2fms  cold p95=%.2fms", warmStats.P95Ms, coldStats.P95Ms)
		}
	}

	// Ceiling sweep.
	log.Printf("running vec0 ceiling sweep ...")
	sweepDB := filepath.Join(filepath.Dir(*dbPath), "bench_sweep.db")
	if err := os.Remove(sweepDB); err != nil && !os.IsNotExist(err) {
		log.Fatalf("remove sweep db: %v", err)
	}
	ceiling, reason, err := bench.RunCeilingSweep(sweepDB, 20, rng)
	if err != nil {
		log.Fatalf("ceiling sweep: %v", err)
	}
	result.Vec0Ceiling = ceiling
	result.CeilingReason = reason
	log.Printf("ceiling: pop=%d reason=%s", ceiling, reason)

	// Write output.
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(*outPath, data, 0o644); err != nil {
		log.Fatalf("write: %v", err)
	}
	log.Printf("results written to %s", *outPath)
}
