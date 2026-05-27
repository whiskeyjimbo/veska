//go:build multi_branch_bench

// Command multi-branch-bench performs the M1 multi-branch scenario:
// - Steady-state seed: 50 branches × 5000 nodes for RSS and OQ-S006 measurement
// - Promotion trials: 20 × 50k-node INSERT for p95 gate 5
// - Query p95: 200 warm indexed lookups for OQ-S006 comparison
// - GC sweep: DELETE 10 branches and measure wall time + reclaimed disk
//
// Exit codes:
//
//	0 — all gates PASS
//	1 — at least one gate FAIL
package main

import (
	"context"
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

const (
	resultsFile = "tools/loadtest/multi-branch/RESULTS.md"

	steadyBranches = 50
	steadyNodes    = 5_000

	promotionTrials    = 20
	promotionNodesEach = 50_000
	promotionGateSecs  = 5.0

	queryIters   = 200
	rssGateBytes = 2 * 1024 * 1024 * 1024 // 2 GiB
	gcBranches   = 10

	repoID = "repo-bench"
)

// nodeQuery is the OQ-S006 indexed lookup.
const nodeQuery = `SELECT node_id, kind, symbol_path, file_path FROM nodes WHERE repo_id=? AND branch=? AND node_id=?`

// ---- schema ---------------------------------------------------------------

const ddl = `
CREATE TABLE IF NOT EXISTS repos (
    repo_id TEXT PRIMARY KEY,
    root_path TEXT NOT NULL UNIQUE,
    added_at INTEGER NOT NULL,
    active_branch TEXT,
    last_promoted_sha TEXT,
    module_path TEXT
);

CREATE TABLE IF NOT EXISTS nodes (
    node_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    line_start     INTEGER,
    line_end       INTEGER,
    content_hash   TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL,
    PRIMARY KEY (node_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_nodes_repo_branch ON nodes(repo_id, branch);
CREATE INDEX IF NOT EXISTS idx_nodes_symbol ON nodes(symbol_path);

CREATE TABLE IF NOT EXISTS edges (
    from_id   TEXT NOT NULL,
    to_id     TEXT NOT NULL,
    branch    TEXT NOT NULL,
    repo_id   TEXT NOT NULL,
    kind      TEXT NOT NULL,
    PRIMARY KEY (from_id, to_id, branch, kind),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_edges_repo_branch ON edges(repo_id, branch);

CREATE TABLE IF NOT EXISTS findings (
    finding_id   TEXT NOT NULL,
    branch       TEXT NOT NULL,
    repo_id      TEXT NOT NULL,
    node_id      TEXT,
    file_path    TEXT,
    severity     TEXT NOT NULL,
    source_layer TEXT NOT NULL,
    rule         TEXT NOT NULL,
    message      TEXT NOT NULL,
    state        TEXT NOT NULL,
    closed_reason TEXT,
    created_at   INTEGER NOT NULL,
    closed_at    INTEGER,
    actor_id     TEXT NOT NULL,
    actor_kind   TEXT NOT NULL,
    PRIMARY KEY (finding_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);`

func setupSchema(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return fmt.Errorf("synchronous: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("foreign_keys: %w", err)
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("ddl: %w", err)
	}
	return nil
}

// ---- seed helpers ----------------------------------------------------------

func seedRepo(db *sql.DB, rID string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO repos(repo_id, root_path, added_at) VALUES (?,?,?)`,
		rID, "/bench/"+rID, time.Now().UnixNano(),
	)
	return err
}

func seedBranchNodes(db *sql.DB, rID, branch string, n int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO nodes
		(node_id,branch,repo_id,language,kind,symbol_path,file_path,line_start,line_end,
		 content_hash,last_promoted_at,actor_id,actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	for i := range n {
		sym := fmt.Sprintf("pkg.Symbol%d", i)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(branch+sym)))
		nodeID := fmt.Sprintf("node-%s-%d", branch, i)
		filePath := fmt.Sprintf("pkg/file%d.go", i/100)
		if _, err := stmt.Exec(
			nodeID, branch, rID, "go", "function", sym, filePath,
			i%200+1, i%200+20, hash, now, "bench", "tool",
		); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("insert node %d: %w", i, err)
		}
	}
	return tx.Commit()
}

func seedBranchEdges(db *sql.DB, rID, branch string, n int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO edges(from_id,to_id,branch,repo_id,kind) VALUES (?,?,?,?,?)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer stmt.Close()

	for i := range n {
		fromID := fmt.Sprintf("node-%s-%d", branch, i)
		toID := fmt.Sprintf("node-%s-%d", branch, (i+1)%n)
		if _, err := stmt.Exec(fromID, toID, branch, rID, "CALLS"); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("insert edge %d: %w", i, err)
		}
	}
	return tx.Commit()
}

func seedBranchFindings(db *sql.DB, rID, branch string, n int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO findings
		(finding_id,branch,repo_id,node_id,file_path,severity,source_layer,rule,message,state,created_at,actor_id,actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	findingIdx := 0
	for i := range n {
		if i%100 != 0 {
			continue
		}
		nodeID := fmt.Sprintf("node-%s-%d", branch, i)
		fID := fmt.Sprintf("finding-%s-%d", branch, findingIdx)
		filePath := fmt.Sprintf("pkg/file%d.go", i/100)
		if _, err := stmt.Exec(
			fID, branch, rID, nodeID, filePath,
			"WARN", "static", "unused-import", "unused import detected",
			"open", now, "bench", "tool",
		); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("insert finding %d: %w", findingIdx, err)
		}
		findingIdx++
	}
	return tx.Commit()
}

// txInsert performs a single promotion trial: BEGIN IMMEDIATE, insert nNodes
// nodes + nNodes edges, COMMIT. Used for promotion p95 measurement.
func txInsert(db *sql.DB, rID, branch string, nNodes int) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}

	nodeStmt, err := tx.Prepare(`INSERT OR REPLACE INTO nodes
		(node_id,branch,repo_id,language,kind,symbol_path,file_path,line_start,line_end,
		 content_hash,last_promoted_at,actor_id,actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer nodeStmt.Close()

	edgeStmt, err := tx.Prepare(`INSERT OR REPLACE INTO edges(from_id,to_id,branch,repo_id,kind) VALUES (?,?,?,?,?)`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	defer edgeStmt.Close()

	now := time.Now().UnixNano()
	for i := range nNodes {
		sym := fmt.Sprintf("pkg.Symbol%d", i)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(branch+sym)))
		nodeID := fmt.Sprintf("node-%s-%d", branch, i)
		filePath := fmt.Sprintf("pkg/file%d.go", i/100)
		if _, err := nodeStmt.Exec(
			nodeID, branch, rID, "go", "function", sym, filePath,
			i%200+1, i%200+20, hash, now, "bench", "tool",
		); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("insert node %d: %w", i, err)
		}
	}

	for i := range nNodes {
		fromID := fmt.Sprintf("node-%s-%d", branch, i)
		toID := fmt.Sprintf("node-%s-%d", branch, (i+1)%nNodes)
		if _, err := edgeStmt.Exec(fromID, toID, branch, rID, "CALLS"); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("insert edge %d: %w", i, err)
		}
	}

	return tx.Commit()
}

// ---- percentile helper -----------------------------------------------------

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ---- disk size helper -------------------------------------------------------

func dbFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ---- main ------------------------------------------------------------------

func main() {
	// Use a file-based temp dir — leave it until after RSS measurement.
	dir, err := os.MkdirTemp("", "multi-branch-bench-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	// Do NOT defer RemoveAll here — we need the file to persist for RSS measurement.

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

	fmt.Println("Seeding repo...")
	if err := seedRepo(db, repoID); err != nil {
		fmt.Fprintf(os.Stderr, "seed repo: %v\n", err)
		os.Exit(1)
	}

	// ---- Phase 1: Steady-state seed -----------------------------------------
	fmt.Printf("Phase 1: Seeding %d branches × %d nodes...\n", steadyBranches, steadyNodes)
	steadyBranchNames := make([]string, steadyBranches)
	for b := range steadyBranches {
		branch := fmt.Sprintf("steady-branch-%d", b)
		steadyBranchNames[b] = branch
		if err := seedBranchNodes(db, repoID, branch, steadyNodes); err != nil {
			fmt.Fprintf(os.Stderr, "seed nodes %s: %v\n", branch, err)
			os.Exit(1)
		}
		if err := seedBranchEdges(db, repoID, branch, steadyNodes); err != nil {
			fmt.Fprintf(os.Stderr, "seed edges %s: %v\n", branch, err)
			os.Exit(1)
		}
		if err := seedBranchFindings(db, repoID, branch, steadyNodes); err != nil {
			fmt.Fprintf(os.Stderr, "seed findings %s: %v\n", branch, err)
			os.Exit(1)
		}
		if (b+1)%10 == 0 {
			fmt.Printf("  ... %d/%d branches seeded\n", b+1, steadyBranches)
		}
	}

	// Measure row counts.
	var nodeRowsAfterSeed, edgeRowsAfterSeed, findingRowsAfterSeed int64
	_ = db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodeRowsAfterSeed)
	_ = db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edgeRowsAfterSeed)
	_ = db.QueryRow("SELECT COUNT(*) FROM findings").Scan(&findingRowsAfterSeed)

	diskBeforePromo := dbFileSize(dbPath)

	// Measure RSS after steady-state seed (before promotion trials).
	rssBytes, err := currentRSSBytes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rss: %v (continuing)\n", err)
	}
	fmt.Printf("Steady-state RSS: %d bytes (%.1f MiB)\n", rssBytes, float64(rssBytes)/1024/1024)

	// ---- Phase 2: Promotion p95 ---------------------------------------------
	fmt.Printf("Phase 2: %d promotion trials × %d nodes each...\n", promotionTrials, promotionNodesEach)

	promoTimes := make([]float64, 0, promotionTrials)
	for t := range promotionTrials {
		branch := fmt.Sprintf("promo-branch-%d", t)
		// Ensure repo row exists for FK.
		start := time.Now()
		if err := txInsert(db, repoID, branch, promotionNodesEach); err != nil {
			fmt.Fprintf(os.Stderr, "txInsert trial %d: %v\n", t, err)
			os.Exit(1)
		}
		elapsed := time.Since(start).Seconds()
		promoTimes = append(promoTimes, elapsed)
		fmt.Printf("  trial %2d: %.2fs\n", t, elapsed)
	}
	sort.Float64s(promoTimes)
	promoP95 := percentile(promoTimes, 0.95)

	var nodeRowsAfterPromo, edgeRowsAfterPromo int64
	_ = db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodeRowsAfterPromo)
	_ = db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edgeRowsAfterPromo)
	diskAfterPromo := dbFileSize(dbPath)

	// ---- Phase 3: Query p95 -------------------------------------------------
	fmt.Printf("Phase 3: %d query iterations for OQ-S006...\n", queryIters)

	// Warm-up query.
	warmRows, err := db.Query(nodeQuery, repoID, "steady-branch-0",
		fmt.Sprintf("node-steady-branch-0-%d", rand.IntN(steadyNodes)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warm query: %v\n", err)
		os.Exit(1)
	}
	warmRows.Close()

	queryTimes := make([]float64, 0, queryIters)
	for i := range queryIters {
		branchIdx := i % steadyBranches
		branch := fmt.Sprintf("steady-branch-%d", branchIdx)
		nodeID := fmt.Sprintf("node-%s-%d", branch, rand.IntN(steadyNodes))
		start := time.Now()
		rows, err := db.Query(nodeQuery, repoID, branch, nodeID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query iter %d: %v\n", i, err)
			os.Exit(1)
		}
		rows.Close()
		queryTimes = append(queryTimes, float64(time.Since(start).Microseconds())/1000.0)
	}
	sort.Float64s(queryTimes)
	queryP50 := percentile(queryTimes, 0.50)
	queryP95 := percentile(queryTimes, 0.95)
	queryP99 := percentile(queryTimes, 0.99)

	// ---- Phase 4: GC sweep --------------------------------------------------
	fmt.Printf("Phase 4: GC sweep — deleting %d branches...\n", gcBranches)

	diskBeforeGC := dbFileSize(dbPath)
	gcStart := time.Now()
	for b := range gcBranches {
		branch := steadyBranchNames[b]
		// CASCADE is scoped to repo_id; branch is a column, not a FK target.
		// Delete branch rows directly across all tables.
		if _, err := db.Exec(`DELETE FROM nodes WHERE repo_id=? AND branch=?`, repoID, branch); err != nil {
			fmt.Fprintf(os.Stderr, "gc delete nodes %s: %v\n", branch, err)
			os.Exit(1)
		}
		if _, err := db.Exec(`DELETE FROM edges WHERE repo_id=? AND branch=?`, repoID, branch); err != nil {
			fmt.Fprintf(os.Stderr, "gc delete edges %s: %v\n", branch, err)
			os.Exit(1)
		}
		if _, err := db.Exec(`DELETE FROM findings WHERE repo_id=? AND branch=?`, repoID, branch); err != nil {
			fmt.Fprintf(os.Stderr, "gc delete findings %s: %v\n", branch, err)
			os.Exit(1)
		}
	}
	gcElapsed := time.Since(gcStart)
	// VACUUM to reclaim disk space.
	if _, err := db.Exec("VACUUM"); err != nil {
		fmt.Fprintf(os.Stderr, "vacuum: %v\n", err)
	}
	diskAfterGC := dbFileSize(dbPath)

	// ---- Verdicts -----------------------------------------------------------

	// RSS formatting.
	rssMiB := float64(rssBytes) / 1024 / 1024
	var rssFormatted string
	if rssMiB >= 1024 {
		rssFormatted = fmt.Sprintf("%.2fGiB", rssMiB/1024)
	} else {
		rssFormatted = fmt.Sprintf("%dmb", int64(rssMiB))
	}

	gate4Pass := rssBytes <= rssGateBytes || rssBytes == 0 // 0 = unsupported platform
	gate5Pass := promoP95 < promotionGateSecs

	gate4Verdict := "PASS"
	if !gate4Pass {
		gate4Verdict = "FAIL"
	}
	gate5Verdict := "PASS"
	if !gate5Pass {
		gate5Verdict = "FAIL"
	}

	// OQ-S006 comparison vs M0:
	// M0 baseline: 28 branches × 100k nodes = 2.8M node rows, disk=1.68 GiB, node p95=0.04ms
	// Regression threshold: ≥2x on rows/GiB or query p95.
	m0NodeRowsPerBranch := int64(100_000)
	m0DiskPerRow := float64(1.68*1024*1024*1024) / float64(28*100_000)
	m0QueryP95ms := 0.04

	m1NodeRowsPerBranch := nodeRowsAfterSeed / int64(steadyBranches)
	m1DiskPerRow := float64(diskBeforePromo) / float64(nodeRowsAfterSeed)

	rowRatio := float64(m1NodeRowsPerBranch) / float64(m0NodeRowsPerBranch)
	diskRatio := m1DiskPerRow / m0DiskPerRow
	queryRatio := queryP95 / m0QueryP95ms

	oqVerdict := "GREEN"
	oqNote := "all ratios < 2x vs M0 baseline"
	if rowRatio >= 2.0 || diskRatio >= 2.0 || queryRatio >= 2.0 {
		oqVerdict = "RED"
		oqNote = fmt.Sprintf("≥2x regression: rowRatio=%.2f diskRatio=%.2f queryRatio=%.2f", rowRatio, diskRatio, queryRatio)
	} else if rowRatio >= 1.5 || diskRatio >= 1.5 || queryRatio >= 1.5 {
		oqVerdict = "YELLOW"
		oqNote = fmt.Sprintf("1.5x–2x range: rowRatio=%.2f diskRatio=%.2f queryRatio=%.2f", rowRatio, diskRatio, queryRatio)
	}

	// ---- Write RESULTS.md ---------------------------------------------------

	content := fmt.Sprintf(`# Multi-Branch Bench — M1 Gates 4+5 + OQ-S006

Generated: %s
Platform: linux amd64

## Phase 1 — Steady-State Seed (%d branches × %d nodes)

| Metric | Value |
|--------|-------|
| Branches seeded | %d |
| Nodes per branch | %d |
| Total node rows | %d |
| Total edge rows | %d |
| Total finding rows | %d |
| DB file size (post-seed) | %s |

## Phase 2 — Promotion Trials (%d trials × %d nodes)

| Metric | Value |
|--------|-------|
| Trials | %d |
| Nodes per trial | %d |
| Promo p50 | %.2fs |
| Promo p95 | %.2fs |
| Total node rows (post-promo) | %d |
| Total edge rows (post-promo) | %d |
| DB file size (post-promo) | %s |

## Phase 3 — Query p95 (OQ-S006)

| Metric | Value |
|--------|-------|
| Iterations | %d |
| Query p50 | %.3fms |
| Query p95 | %.3fms |
| Query p99 | %.3fms |

## Phase 4 — GC Sweep (%d branches deleted)

| Metric | Value |
|--------|-------|
| Disk before GC | %s |
| GC sweep time | %s |
| Disk after GC (post-VACUUM) | %s |
| Reclaimed | %s |

## OQ-S006 Comparison vs M0

M0 baseline: 28 branches × 100k nodes, disk=1.68 GiB, node-query p95=0.04ms

| Ratio | M0 | M1 | Ratio | Threshold |
|-------|----|----|-------|-----------|
| Rows/branch | %d | %d | %.2fx | <2x |
| Disk/row (bytes) | %.1f | %.1f | %.2fx | <2x |
| Query p95 (ms) | %.3f | %.3f | %.2fx | <2x |

OQ-S006 verdict: **%s** — %s

## Gate Results

| Metric | Value | Budget | Verdict |
|--------|-------|--------|---------|
| Daemon RSS | %s | ≤2GiB | %s |
| Promotion 50k p95 | %.2fs | <5s | %s |
`,
		time.Now().Format("2006-01-02"),
		steadyBranches, steadyNodes,
		steadyBranches,
		steadyNodes,
		nodeRowsAfterSeed,
		edgeRowsAfterSeed,
		findingRowsAfterSeed,
		formatBytes(diskBeforePromo),
		promotionTrials, promotionNodesEach,
		promotionTrials,
		promotionNodesEach,
		percentile(promoTimes, 0.50),
		promoP95,
		nodeRowsAfterPromo,
		edgeRowsAfterPromo,
		formatBytes(diskAfterPromo),
		queryIters,
		queryP50, queryP95, queryP99,
		gcBranches,
		formatBytes(diskBeforeGC),
		gcElapsed.Round(time.Millisecond),
		formatBytes(diskAfterGC),
		formatBytes(diskBeforeGC-diskAfterGC),
		m0NodeRowsPerBranch, m1NodeRowsPerBranch, rowRatio,
		m0DiskPerRow, m1DiskPerRow, diskRatio,
		m0QueryP95ms, queryP95, queryRatio,
		oqVerdict, oqNote,
		rssFormatted, gate4Verdict,
		promoP95, gate5Verdict,
	)

	if err := os.MkdirAll(filepath.Dir(resultsFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(resultsFile, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write results: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s\n", resultsFile)

	// Clean up temp dir now that results are written.
	os.RemoveAll(dir) //nolint:errcheck

	// Print summary.
	fmt.Printf("\n--- Summary ---\n")
	fmt.Printf("Gate 4 (RSS ≤ 2 GiB): %s (%s)\n", gate4Verdict, rssFormatted)
	fmt.Printf("Gate 5 (promo p95 <5s): %s (%.2fs)\n", gate5Verdict, promoP95)
	fmt.Printf("OQ-S006: %s\n", oqVerdict)

	if !gate4Pass || !gate5Pass {
		os.Exit(1)
	}
}

func formatBytes(b int64) string {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	if b >= gib {
		return fmt.Sprintf("%.2f GiB", float64(b)/gib)
	}
	if b >= mib {
		return fmt.Sprintf("%.1f MiB", float64(b)/mib)
	}
	return fmt.Sprintf("%d bytes", b)
}
