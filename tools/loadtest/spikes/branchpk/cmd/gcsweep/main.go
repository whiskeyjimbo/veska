// Command gcsweep benchmarks branch-GC sweep on a SQLite DB:
// deletes N branches, records wall-clock time, disk reclaim, and WAL sizes.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/gcsweep"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/pkloader"
)

func main() {
	symbols := flag.Int("symbols", 100000, "total symbols (distributed across branches)")
	branches := flag.Int("branches", 50, "number of branches to generate")
	deleteN := flag.Int("delete", 10, "number of branches to delete")
	overlap := flag.Int("overlap", 10, "dirty-overlap percent per branch")
	dbFlag := flag.String("db", "data/gc_test.db", "path to SQLite DB (created if absent)")
	outFlag := flag.String("out", "data/gcsweep_metrics.json", "output JSON file path")
	flag.Parse()

	if *deleteN > *branches {
		fmt.Fprintf(os.Stderr, "error: -delete (%d) cannot exceed -branches (%d)\n", *deleteN, *branches)
		os.Exit(1)
	}

	dbPath, err := filepath.Abs(*dbFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve db path: %v\n", err)
		os.Exit(1)
	}

	// Create DB if it doesn't exist.
	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "DB not found at %s — generating with %d branches, %d symbols, %d%% overlap\n",
			dbPath, *branches, *symbols, *overlap)
		if err := generate(dbPath, *branches, *symbols, *overlap); err != nil {
			fmt.Fprintf(os.Stderr, "generate: %v\n", err)
			os.Exit(1)
		}
	}

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// List all distinct branches.
	allBranches, err := listBranches(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list branches: %v\n", err)
		os.Exit(1)
	}

	if *deleteN > len(allBranches) {
		fmt.Fprintf(os.Stderr, "warning: -delete (%d) exceeds actual branch count (%d), deleting all\n",
			*deleteN, len(allBranches))
		*deleteN = len(allBranches)
	}

	// Keep all branches except the first deleteN.
	keepBranches := allBranches[*deleteN:]
	fmt.Fprintf(os.Stderr, "GC sweep: %d branches present, deleting first %d, keeping %d\n",
		len(allBranches), *deleteN, len(keepBranches))

	result, err := gcsweep.RunGCSweep(db, dbPath, keepBranches)
	if err != nil {
		fmt.Fprintf(os.Stderr, "RunGCSweep: %v\n", err)
		os.Exit(1)
	}

	// Write JSON output.
	outPath, err := filepath.Abs(*outFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve out path: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	data, err := json.MarshalIndent([]gcsweep.GCSweepResult{result}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stdout, "%s\n", data)
	fmt.Fprintf(os.Stderr, "Metrics written to %s\n", outPath)
}

// generate creates the DB and populates it with the requested branches and symbols.
func generate(dbPath string, nBranches, nSymbols, overlapPct int) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	// Use WAL for better performance during load.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("set WAL: %w", err)
	}
	// Deferred FK checks allow circular edge inserts inside transactions.
	if _, err := db.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		return fmt.Errorf("defer_foreign_keys: %w", err)
	}

	if err := pkloader.CreateSchema(db); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if err := pkloader.InsertRepo(db, "repo1"); err != nil {
		return fmt.Errorf("insert repo: %w", err)
	}

	symbolsPerBranch := max(1, nSymbols/nBranches)

	baseSymbols := pkloader.GenerateBaseSymbols(symbolsPerBranch, "repo1")
	ts := time.Now().Unix()

	for i := range nBranches {
		branch := fmt.Sprintf("branch-%04d", i)
		var syms []pkloader.Symbol
		if overlapPct > 0 {
			syms = pkloader.ApplyDirtyOverlap(baseSymbols, overlapPct, uint64(i+1))
		} else {
			syms = baseSymbols
		}
		if err := pkloader.InsertBranch(db, branch, "repo1", syms, ts); err != nil {
			return fmt.Errorf("insert branch %s: %w", branch, err)
		}
		if i%10 == 0 {
			fmt.Fprintf(os.Stderr, "  loaded branch %d/%d\n", i+1, nBranches)
		}
	}
	return nil
}

// listBranches returns distinct branch names from the nodes table.
func listBranches(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT branch FROM nodes ORDER BY branch`)
	if err != nil {
		return nil, fmt.Errorf("query branches: %w", err)
	}
	defer rows.Close()

	var branches []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("scan branch: %w", err)
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}
