// Command loader loads synthetic 768-dim vectors into a sqlite-vec vec0 virtual table
// for two populations (50k and 1M), recording wall-clock time, disk footprint, and
// peak RSS. Metrics are written as a JSON array to the output file.
// Usage:
//
//	loader [-n50k 50000] [-n1m 1000000] [-db data/spike.db] [-out data/load_metrics.json]
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"time"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
)

func main() {
	n50k := flag.Int("n50k", 50_000, "population size for first pass")
	n1m := flag.Int("n1m", 1_000_000, "population size for second pass")
	dbPath := flag.String("db", "data/spike.db", "sqlite database file path")
	outPath := flag.String("out", "data/load_metrics.json", "output JSON metrics file")
	flag.Parse()

	populations := []int{*n50k, *n1m}
	var metrics []loader.LoadMetrics

	for _, pop := range populations {
		fmt.Printf("=== Population: %d vectors ===\n", pop)

		// Remove old DB to start fresh for each population.
		if err := os.Remove(*dbPath); err != nil && !os.IsNotExist(err) {
			log.Fatalf("remove old db: %v", err)
		}
		// Also remove WAL and SHM files.
		_ = os.Remove(*dbPath + "-wal")
		_ = os.Remove(*dbPath + "-shm")

		seed := rand.Uint64()
		fmt.Printf("  Generating %d vectors (seed=%d)...\n", pop, seed)
		vecs := gen.GenerateVectors(pop, seed)

		fmt.Printf("  Opening database at %s...\n", *dbPath)
		l, err := loader.Open(*dbPath)
		if err != nil {
			log.Fatalf("loader.Open: %v", err)
		}

		rssBefore := loader.ReadRSSBytes()

		start := time.Now()
		fmt.Printf("  Inserting %d vectors...\n", pop)

		// Insert in chunks of 10k to keep transaction size manageable.
		const chunkSize = 10_000
		for i := 0; i < len(vecs); i += chunkSize {
			end := min(i+chunkSize, len(vecs))
			if err := l.InsertBatch(vecs[i:end]); err != nil {
				log.Fatalf("InsertBatch chunk %d-%d: %v", i, end, err)
			}
			fmt.Printf("    inserted %d / %d\r", end, pop)
		}
		fmt.Println()

		wallMs := time.Since(start).Milliseconds()

		rssAfter := loader.ReadRSSBytes()
		peakRSS := rssAfter
		if rssBefore > 0 && rssAfter > rssBefore {
			peakRSS = rssAfter
		}

		if err := l.Checkpoint(); err != nil {
			log.Fatalf("Checkpoint: %v", err)
		}

		diskBytes, err := l.DiskBytes()
		if err != nil {
			log.Fatalf("DiskBytes: %v", err)
		}

		rowCount, err := l.RowCount()
		if err != nil {
			log.Fatalf("RowCount: %v", err)
		}

		l.Close()

		m := loader.LoadMetrics{
			Population:   int64(pop),
			LoadWallMs:   wallMs,
			DiskBytes:    diskBytes,
			PeakRSSBytes: peakRSS,
		}
		metrics = append(metrics, m)

		fmt.Printf("  rows=%d  wall=%dms  disk=%d bytes  rss=%d bytes\n",
			rowCount, wallMs, diskBytes, peakRSS)
	}

	fmt.Printf("\nWriting metrics to %s...\n", *outPath)
	if err := loader.WriteMetricsJSON(*outPath, metrics); err != nil {
		log.Fatalf("WriteMetricsJSON: %v", err)
	}
	fmt.Println("Done.")
}
