// SPDX-License-Identifier: AGPL-3.0-only

// Command recall runs recall@10 and recall@50 benchmarks against sqlite-vec KNN queries.
// It generates a corpus at each population size, creates a hold-out set of query vectors,
// computes ground-truth nearest neighbors via brute force, queries vec0, and reports recall.
// Results are written as a JSON array of RecallResult to the -out file.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/loader"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/recall"
)

func init() {
	vec.Auto()
}

func main() {
	n50k := flag.Int("n50k", 50_000, "corpus size for the 50k population run")
	n1m := flag.Int("n1m", 1_000_000, "corpus size for the 1M population run")
	holdOutN := flag.Int("holdout", 100, "number of hold-out query vectors")
	out := flag.String("out", "data/recall_metrics.json", "output path for JSON results")
	dbPrefix := flag.String("db-prefix", "data/recall", "path prefix for temporary SQLite DBs")
	flag.Parse()

	populations := []int64{int64(*n50k), int64(*n1m)}
	results := make([]recall.RecallResult, 0, len(populations))

	for _, pop := range populations {
		log.Printf("recall: starting population=%d hold-out=%d", pop, *holdOutN)

		dbPath := fmt.Sprintf("%s_%d.db", *dbPrefix, pop)

		// Remove any stale DB from a previous run.
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")

		// Generate corpus and insert into the DB.
		const corpusSeed uint64 = 0xc0de5eed1234
		corpus := gen.GenerateVectors(int(pop), corpusSeed)
		log.Printf("recall: generated %d corpus vectors", len(corpus))

		l, err := loader.Open(dbPath)
		if err != nil {
			log.Fatalf("recall: open db for pop %d: %v", pop, err)
		}

		const batchSize = 10_000
		for i := 0; i < len(corpus); i += batchSize {
			end := min(i+batchSize, len(corpus))
			if err := l.InsertBatch(corpus[i:end]); err != nil {
				l.Close()
				log.Fatalf("recall: insert batch at pop %d: %v", pop, err)
			}
		}
		l.Close()
		log.Printf("recall: inserted corpus into %s", dbPath)

		// Re-open for querying (bench.QueryVec0 needs *sql.DB).
		db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
		if err != nil {
			log.Fatalf("recall: open db for queries at pop %d: %v", pop, err)
		}
		db.SetMaxOpenConns(1)

		// Generate hold-out (different seed from corpus).
		const holdOutSeed uint64 = 0xd15a57a5eed
		holdOut := recall.GenerateHoldOut(*holdOutN, holdOutSeed)
		log.Printf("recall: generated %d hold-out vectors", len(holdOut))

		res, err := recall.RunRecall(db, corpus, holdOut, pop)
		db.Close()
		if err != nil {
			log.Fatalf("recall: RunRecall at pop %d: %v", pop, err)
		}

		log.Printf("recall: pop=%d recall@10=%.4f recall@50=%.4f",
			pop, res.RecallAt10, res.RecallAt50)

		results = append(results, res)
	}

	// Write results JSON.
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatalf("recall: mkdir for output: %v", err)
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("recall: marshal results: %v", err)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		log.Fatalf("recall: write output: %v", err)
	}
	log.Printf("recall: results written to %s", *out)
}
