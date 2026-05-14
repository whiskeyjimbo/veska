// Command bench measures warm indexed-lookup latency for the branchpk SQLite schema
// and compares the results against SOLO-13 §3.1 budgets.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/bench"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/pkloader"
)

func main() {
	symbols := flag.Int("symbols", 100000, "number of base symbols")
	branches := flag.Int("branches", 50, "number of branches")
	queries := flag.Int("queries", 200, "warm queries per benchmark")
	overlap := flag.Int("overlap", 10, "dirty-overlap percentage")
	dbPath := flag.String("db", "data/branchpk_10pct.db", "SQLite database path")
	outPath := flag.String("out", "data/bench_metrics.json", "JSON output file (array-appended)")
	flag.Parse()

	db, err := openOrGenerate(*dbPath, *symbols, *branches, *overlap)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	// Warm the OS page cache with a couple of throwaway queries.
	_ = bench.GetNode(db, "warmup", "main")

	syms := pkloader.GenerateBaseSymbols(*symbols, "repo0")
	bnames := makeBranchNames(*branches)
	rng := rand.New(rand.NewSource(42))

	nodeStats, err := bench.RunNodeBench(db, syms, bnames, *queries, rng)
	if err != nil {
		log.Fatalf("RunNodeBench: %v", err)
	}

	edgesStats, err := bench.RunEdgesBench(db, syms, bnames, *queries, rng)
	if err != nil {
		log.Fatalf("RunEdgesBench: %v", err)
	}

	result := bench.BenchResult{
		OverlapPct:    *overlap,
		Branches:      *branches,
		Symbols:       *symbols,
		NodeLatency:   nodeStats,
		EdgesLatency:  edgesStats,
		NodeBudgetMs:  bench.NodeBudgetMs,
		EdgesBudgetMs: bench.EdgesBudgetMs,
		NodePass:      nodeStats.P95Ms < bench.NodeBudgetMs,
		EdgesPass:     edgesStats.P95Ms < bench.EdgesBudgetMs,
	}

	if err := appendJSON(*outPath, result); err != nil {
		log.Fatalf("write output: %v", err)
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))

	if !result.NodePass {
		fmt.Fprintf(os.Stderr, "WARN: node p95 %.2fms exceeds budget %.0fms\n",
			result.NodeLatency.P95Ms, bench.NodeBudgetMs)
	}
	if !result.EdgesPass {
		fmt.Fprintf(os.Stderr, "WARN: edges p95 %.2fms exceeds budget %.0fms\n",
			result.EdgesLatency.P95Ms, bench.EdgesBudgetMs)
	}
}

// openOrGenerate opens an existing DB or generates it inline using pkloader.
func openOrGenerate(dbPath string, numSymbols, numBranches, overlapPct int) (*sql.DB, error) {
	exists := true
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		exists = false
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=65536", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	if !exists {
		log.Printf("generating %s (symbols=%d branches=%d overlap=%d%%)", dbPath, numSymbols, numBranches, overlapPct)
		if err := generate(db, numSymbols, numBranches, overlapPct); err != nil {
			db.Close()
			return nil, fmt.Errorf("generate: %w", err)
		}
	} else {
		log.Printf("using existing db: %s", dbPath)
	}

	return db, nil
}

func generate(db *sql.DB, numSymbols, numBranches, overlapPct int) error {
	if err := pkloader.CreateSchema(db); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	const repoID = "repo0"
	if err := pkloader.InsertRepo(db, repoID); err != nil {
		return fmt.Errorf("repo: %w", err)
	}

	base := pkloader.GenerateBaseSymbols(numSymbols, repoID)
	branches := makeBranchNames(numBranches)
	var ts int64 = 1700000000

	for i, br := range branches {
		syms := pkloader.ApplyDirtyOverlap(base, overlapPct, uint64(i+1))
		if err := pkloader.InsertBranch(db, br, repoID, syms, ts+int64(i)); err != nil {
			return fmt.Errorf("branch %s: %w", br, err)
		}
	}
	return nil
}

func makeBranchNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("branch-%04d", i)
	}
	return names
}

// appendJSON appends result to a JSON array in path (creating the file if needed).
func appendJSON(path string, result bench.BenchResult) error {
	var records []bench.BenchResult

	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &records)
	}
	records = append(records, result)

	out, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, out, 0o644)
}
