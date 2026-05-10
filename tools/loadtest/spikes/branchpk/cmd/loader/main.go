// Command loader runs synthetic branch-in-PK schema load tests at 10%, 30%, and 60%
// dirty overlap and emits a JSON metrics array.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/branchpk/pkloader"
)

func main() {
	symbols := flag.Int("symbols", 100000, "number of base symbols")
	branches := flag.Int("branches", 50, "number of branches per overlap setting")
	out := flag.String("out", "data/load_metrics.json", "path to write JSON metrics array")
	dbPrefix := flag.String("db-prefix", "data/branchpk", "path prefix for DB files (suffix: _<overlap>.db)")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(*out), err)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPrefix), 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(*dbPrefix), err)
	}

	overlapSettings := []int{10, 30, 60}
	results := make([]pkloader.LoadMetrics, 0, len(overlapSettings))

	for _, overlap := range overlapSettings {
		m, err := runLoad(*dbPrefix, overlap, *symbols, *branches)
		if err != nil {
			log.Fatalf("runLoad overlap=%d: %v", overlap, err)
		}
		results = append(results, m)
		printMetrics(m)
	}

	b, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		log.Fatalf("marshal metrics: %v", err)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("Metrics written to %s\n", *out)
}

func runLoad(dbPrefix string, overlapPct, nSymbols, nBranches int) (pkloader.LoadMetrics, error) {
	dbPath := fmt.Sprintf("%s_%d.db", dbPrefix, overlapPct)
	walPath := dbPath + "-wal"

	// Remove old DB files for a fresh run.
	_ = os.Remove(dbPath)
	_ = os.Remove(walPath)
	_ = os.Remove(dbPath + "-shm")

	dsn := fmt.Sprintf(
		"file:%s?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=-65536&_foreign_keys=0",
		dbPath,
	)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	// Single writer: serialise all work through one connection.
	db.SetMaxOpenConns(1)

	if err := pkloader.CreateSchema(db); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("create schema: %w", err)
	}

	repoID := fmt.Sprintf("repo-overlap%d", overlapPct)
	if err := pkloader.InsertRepo(db, repoID); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("insert repo: %w", err)
	}

	baseSymbols := pkloader.GenerateBaseSymbols(nSymbols, repoID)
	ts := time.Now().Unix()

	start := time.Now()
	for b := range nBranches {
		branch := fmt.Sprintf("branch-%04d", b)
		dirty := pkloader.ApplyDirtyOverlap(baseSymbols, overlapPct, uint64(b+1)*0xdeadbeef)
		if err := pkloader.InsertBranch(db, branch, repoID, dirty, ts); err != nil {
			return pkloader.LoadMetrics{}, fmt.Errorf("insert branch %s: %w", branch, err)
		}
	}
	elapsed := time.Since(start)

	// Re-enable FK enforcement and checkpoint WAL.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("re-enable FK: %w", err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("wal checkpoint: %w", err)
	}

	// Count rows.
	var nodeRows, edgeRows, findingRows int64
	if err := db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodeRows); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("count nodes: %w", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edgeRows); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("count edges: %w", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM findings").Scan(&findingRows); err != nil {
		return pkloader.LoadMetrics{}, fmt.Errorf("count findings: %w", err)
	}

	db.Close()

	dbBytes := fileSize(dbPath)
	walBytes := fileSize(walPath)
	rss := pkloader.ReadRSSBytes()

	return pkloader.LoadMetrics{
		OverlapPct:   overlapPct,
		Branches:     nBranches,
		Symbols:      nSymbols,
		NodeRows:     nodeRows,
		EdgeRows:     edgeRows,
		FindingRows:  findingRows,
		DBBytes:      dbBytes,
		WALBytes:     walBytes,
		PeakRSSBytes: rss,
		LoadWallMs:   elapsed.Milliseconds(),
	}, nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func printMetrics(m pkloader.LoadMetrics) {
	fmt.Printf(
		"overlap=%d%%  branches=%d  symbols=%d  nodes=%d  edges=%d  findings=%d  db=%.1fMB  wal=%.1fMB  rss=%.1fMB  wall=%dms\n",
		m.OverlapPct, m.Branches, m.Symbols,
		m.NodeRows, m.EdgeRows, m.FindingRows,
		float64(m.DBBytes)/1e6, float64(m.WALBytes)/1e6, float64(m.PeakRSSBytes)/1e6,
		m.LoadWallMs,
	)
}
