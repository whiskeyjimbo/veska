//go:build mcp_latency_bench

// Command mcp-latency benchmarks the find_symbol warm p95 query against a
// file-based SQLite database seeded with 50,000 nodes.
// Exit codes:
//
//	0 — p95 < 50ms (PASS)
//	1 — p95 >= 50ms (FAIL)
package main

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

const findSymbolQuery = `
SELECT node_id, branch, repo_id, kind, symbol_path, file_path, line_start, line_end
FROM nodes
WHERE repo_id = ? AND branch = ? AND symbol_path = ?
`

const ddlRepos = `
CREATE TABLE IF NOT EXISTS repos (
    repo_id           TEXT PRIMARY KEY,
    root_path         TEXT NOT NULL UNIQUE,
    added_at          INTEGER NOT NULL,
    active_branch     TEXT,
    last_promoted_sha TEXT,
    module_path       TEXT
);`

const ddlNodes = `
CREATE TABLE IF NOT EXISTS nodes (
    node_id          TEXT NOT NULL,
    branch           TEXT NOT NULL,
    repo_id          TEXT NOT NULL,
    language         TEXT NOT NULL,
    kind             TEXT NOT NULL,
    symbol_path      TEXT NOT NULL,
    file_path        TEXT NOT NULL,
    line_start       INTEGER,
    line_end         INTEGER,
    content_hash     TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    actor_id         TEXT NOT NULL,
    actor_kind       TEXT NOT NULL,
    PRIMARY KEY (node_id, branch)
);
CREATE INDEX IF NOT EXISTS idx_nodes_repo_branch ON nodes(repo_id, branch);
CREATE INDEX IF NOT EXISTS idx_nodes_symbol ON nodes(symbol_path);
`

const (
	repoID      = "repo-bench"
	branch      = "main"
	nNodes      = 50_000
	nIters      = 1_000
	gateMs      = 50.0
	resultsFile = "tools/loadtest/mcp-latency/RESULTS.md"
)

func setupSchema(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("WAL: %w", err)
	}
	if _, err := db.Exec(ddlRepos); err != nil {
		return fmt.Errorf("ddl repos: %w", err)
	}
	if _, err := db.Exec(ddlNodes); err != nil {
		return fmt.Errorf("ddl nodes: %w", err)
	}
	return nil
}

func seedNodes(db *sql.DB, repoID, branch string, n int) error {
	// Insert repo row (required by logical FK).
	_, err := db.Exec(`INSERT OR IGNORE INTO repos(repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/bench/"+repoID, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("insert repo: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO nodes
		(node_id,branch,repo_id,language,kind,symbol_path,file_path,line_start,line_end,content_hash,last_promoted_at,actor_id,actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	for i := range n {
		sym := fmt.Sprintf("pkg.Symbol%d", i)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(sym)))
		nodeID := fmt.Sprintf("node-%d", i)
		filePath := fmt.Sprintf("pkg/file%d.go", i/100)
		if _, err := stmt.Exec(nodeID, branch, repoID, "go", "function", sym, filePath,
			i%200+1, i%200+20, hash, now, "bench", "tool"); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("insert node %d: %w", i, err)
		}
	}
	return tx.Commit()
}

func main() {
	// Use a file-based temp DB for realistic WAL behaviour.
	dir, err := os.MkdirTemp("", "mcp-latency-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "bench.db")
	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("Setting up schema...")
	if err := setupSchema(db); err != nil {
		fmt.Fprintf(os.Stderr, "schema: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Seeding %d nodes...\n", nNodes)
	if err := seedNodes(db, repoID, branch, nNodes); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}

	// Warm run — discard result.
	warmRows, err := db.Query(findSymbolQuery, repoID, branch, "pkg.Symbol25000")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warm query: %v\n", err)
		os.Exit(1)
	}
	warmRows.Close()

	fmt.Printf("Running %d benchmark iterations...\n", nIters)
	latencies := make([]float64, 0, nIters)
	for range nIters {
		sym := fmt.Sprintf("pkg.Symbol%d", rand.IntN(nNodes))
		start := time.Now()
		rows, err := db.Query(findSymbolQuery, repoID, branch, sym)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query: %v\n", err)
			os.Exit(1)
		}
		rows.Close()
		latencies = append(latencies, float64(time.Since(start).Microseconds())/1000.0)
	}

	sort.Float64s(latencies)
	p95idx := int(float64(len(latencies)) * 0.95)
	p95ms := latencies[p95idx]

	verdict := "PASS"
	exitCode := 0
	if p95ms >= gateMs {
		verdict = "FAIL"
		exitCode = 1
	}

	fmt.Printf("p95 = %.3fms  gate=<%.0fms  %s\n", p95ms, gateMs, verdict)

	// Write RESULTS.md.
	content := fmt.Sprintf(`# MCP Latency Benchmark — find_symbol warm p95

Generated: %s
Platform: linux amd64
Nodes seeded: %d
Iterations: %d

| Metric | Value | Gate | Verdict |
|--------|-------|------|---------|
| find_symbol warm p95 | %.3fms | <50ms | %s |

Gate: %s (p95 < 50ms)
`, time.Now().Format("2006-01-02"), nNodes, nIters, p95ms, verdict, verdict)

	if err := os.MkdirAll(filepath.Dir(resultsFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(resultsFile, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write results: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s\n", resultsFile)

	os.Exit(exitCode)
}
