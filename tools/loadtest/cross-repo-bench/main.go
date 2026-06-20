// SPDX-License-Identifier: AGPL-3.0-only

//go:build cross_repo_bench

// Command cross-repo-bench measures the p95 latency of ResolveStubsForNode
// against a seeded in-memory SQLite database with two repos (service + sdk),
// 1000 nodes in each, and 200 cross_repo_edge_stubs from service to sdk.
// Usage:
//
//	go run -tags cross_repo_bench./tools/loadtest/cross-repo-bench/
package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"text/template"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/resolver"
)

const (
	numNodes    = 1000
	numStubs    = 200
	numRuns     = 1000
	resultsPath = "tools/loadtest/cross-repo-bench/RESULTS.md"

	greenThresholdNs  = 50 * int64(time.Millisecond)  // < 50ms
	yellowThresholdNs = 150 * int64(time.Millisecond) // 50–150ms; > 150ms = RED
)

func setupDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	stmts := []string{
		`CREATE TABLE repos (
			repo_id        TEXT PRIMARY KEY,
			root_path      TEXT NOT NULL,
			added_at       INTEGER NOT NULL DEFAULT 0,
			active_branch  TEXT,
			last_promoted_sha TEXT,
			module_path    TEXT
		)`,
		`CREATE TABLE nodes (
			node_id          TEXT NOT NULL,
			branch           TEXT NOT NULL,
			repo_id          TEXT NOT NULL,
			language         TEXT NOT NULL,
			kind             TEXT NOT NULL,
			symbol_path      TEXT NOT NULL,
			file_path        TEXT NOT NULL,
			line_start       INTEGER,
			line_end         INTEGER,
			content_hash     TEXT NOT NULL DEFAULT '',
			last_promoted_at INTEGER NOT NULL DEFAULT 0,
			actor_id         TEXT NOT NULL DEFAULT '',
			actor_kind       TEXT NOT NULL DEFAULT 'system',
			PRIMARY KEY (node_id, branch)
		)`,
		`CREATE TABLE cross_repo_edge_stubs (
			stub_id          TEXT NOT NULL,
			branch           TEXT NOT NULL,
			repo_id          TEXT NOT NULL,
			src_node_id      TEXT NOT NULL,
			kind             TEXT NOT NULL,
			module_path      TEXT NOT NULL,
			symbol_path      TEXT NOT NULL,
			language         TEXT NOT NULL,
			last_promoted_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (stub_id, branch)
		)`,
		`CREATE INDEX idx_stubs_resolver ON cross_repo_edge_stubs(language, module_path, symbol_path)`,
		`CREATE INDEX idx_nodes_resolve ON nodes(repo_id, symbol_path, language, branch)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return nil, fmt.Errorf("setup stmt: %w\nSQL: %s", err, s)
		}
	}
	return db, nil
}

func seedData(db *sql.DB) (stubSrcNodeIDs []string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Seed two repos.
	repos := []struct {
		id         string
		rootPath   string
		modulePath string
	}{
		{"repo-service", "/workspace/service", "github.com/example/service"},
		{"repo-sdk", "/workspace/sdk", "github.com/example/sdk"},
	}
	for _, r := range repos {
		if _, err = tx.Exec(
			`INSERT INTO repos (repo_id, root_path, active_branch, module_path) VALUES (?, ?, 'main', ?)`,
			r.id, r.rootPath, r.modulePath,
		); err != nil {
			return nil, fmt.Errorf("seed repo %s: %w", r.id, err)
		}
	}

	// Seed 1000 nodes in each repo.
	for _, repoID := range []string{"repo-service", "repo-sdk"} {
		for i := 0; i < numNodes; i++ {
			nodeID := fmt.Sprintf("%s-node-%04d", repoID, i)
			symbolPath := fmt.Sprintf("pkg.Symbol%04d", i)
			filePath := fmt.Sprintf("pkg/file%04d.go", i)
			if _, err = tx.Exec(
				`INSERT INTO nodes (node_id, branch, repo_id, language, kind, symbol_path, file_path) VALUES (?, 'main', ?, 'go', 'func', ?, ?)`,
				nodeID, repoID, symbolPath, filePath,
			); err != nil {
				return nil, fmt.Errorf("seed node %s: %w", nodeID, err)
			}
		}
	}

	// Seed 200 cross_repo_edge_stubs from service nodes pointing at sdk symbols.
	// Spread stubs across 50 service nodes (4 stubs each) for realistic spread.
	stubSrcNodeIDs = make([]string, 0, 50)
	seenSrc := make(map[string]bool)
	for i := 0; i < numStubs; i++ {
		srcIdx := (i / 4) % numNodes // 4 stubs per source node, cycling through nodes
		srcNodeID := fmt.Sprintf("repo-service-node-%04d", srcIdx)
		dstIdx := i % numNodes // point at sdk nodes 0.199
		symbolPath := fmt.Sprintf("pkg.Symbol%04d", dstIdx)
		stubID := fmt.Sprintf("stub-%04d", i)

		if _, err = tx.Exec(
			`INSERT INTO cross_repo_edge_stubs (stub_id, branch, repo_id, src_node_id, kind, module_path, symbol_path, language)
			 VALUES (?, 'main', 'repo-service', ?, 'calls', 'github.com/example/sdk', ?, 'go')`,
			stubID, srcNodeID, symbolPath,
		); err != nil {
			return nil, fmt.Errorf("seed stub %s: %w", stubID, err)
		}

		if !seenSrc[srcNodeID] {
			seenSrc[srcNodeID] = true
			stubSrcNodeIDs = append(stubSrcNodeIDs, srcNodeID)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit seed tx: %w", err)
	}
	return stubSrcNodeIDs, nil
}

func p95(durations []time.Duration) time.Duration {
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func verdict(p95ns int64) string {
	switch {
	case p95ns < greenThresholdNs:
		return "GREEN"
	case p95ns < yellowThresholdNs:
		return "YELLOW"
	default:
		return "RED"
	}
}

func main() {
	ctx := context.Background()

	fmt.Println("=== Cross-Repo p95 Bench ===")
	fmt.Printf("Seeding: 2 repos, %d nodes each, %d cross_repo_edge_stubs ...\n", numNodes, numStubs)

	db, err := setupDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	srcNodeIDs, err := seedData(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed data: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Seeded. Distinct stub src nodes: %d\n", len(srcNodeIDs))

	// Warm up (not counted).
	for i := 0; i < 10; i++ {
		nodeID := srcNodeIDs[rand.Intn(len(srcNodeIDs))]
		if _, err := resolver.ResolveStubsForNode(ctx, db, nodeID, "main", false); err != nil {
			fmt.Fprintf(os.Stderr, "warmup resolve: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("Running %d resolve calls ...\n", numRuns)
	durations := make([]time.Duration, 0, numRuns)
	for i := 0; i < numRuns; i++ {
		nodeID := srcNodeIDs[rand.Intn(len(srcNodeIDs))]
		t0 := time.Now()
		_, err := resolver.ResolveStubsForNode(ctx, db, nodeID, "main", false)
		elapsed := time.Since(t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve error: %v\n", err)
			os.Exit(1)
		}
		durations = append(durations, elapsed)
	}

	measuredP95 := p95(durations)
	v := verdict(int64(measuredP95))

	fmt.Printf("\ncross_repo_resolve p95=%s (N=%d) - %s\n", measuredP95.Round(time.Microsecond), numRuns, v)

	if err := writeResults(measuredP95, v); err != nil {
		fmt.Fprintf(os.Stderr, "write results: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Results written to %s\n", resultsPath)
}

const resultsTmpl = `# Cross-Repo p95 Bench - ResolveStubsForNode

Generated: {{.Date}}
Platform: {{.OS}} {{.Arch}}
Repos: 2 (repo-service, repo-sdk)
Nodes per repo: {{.NumNodes}}
Cross-repo stubs: {{.NumStubs}}
Runs: {{.NumRuns}}

## Results

| Metric | Value |
|--------|-------|
| p95 latency | {{.P95}} |
| Verdict | {{.Verdict}} |

## Thresholds

| Color | Threshold |
|--------|-----------|
| GREEN  | p95 < 50ms |
| YELLOW | 50ms ≤ p95 < 150ms |
| RED    | p95 ≥ 150ms |

## OQ-S010 Resolution

OQ-S010 asked: is per-hop cross-repo resolve latency acceptable for interactive use?

Measured p95 = {{.P95}} - **{{.Verdict}}**

{{- if eq .Verdict "GREEN"}}

Result is within the GREEN threshold (< 50ms p95). OQ-S010 is **RESOLVED - no cache ADR required before M2**.
{{- else if eq .Verdict "YELLOW"}}

Result is in the YELLOW band (50–150ms p95). Per SOLO-11 §9: a cache ADR must be filed before M2 closes. OQ-S010 is **RESOLVED - YELLOW, cache ADR required**.
{{- else}}

Result exceeds the RED threshold (≥ 150ms p95). Per SOLO-11 §9: a cache ADR is MANDATORY before M2 closes. OQ-S010 is **RESOLVED - RED, cache ADR mandatory**.
{{- end}}
`

type resultsData struct {
	Date     string
	OS       string
	Arch     string
	NumNodes int
	NumStubs int
	NumRuns  int
	P95      string
	Verdict  string
}

func writeResults(measuredP95 time.Duration, v string) error {
	data := resultsData{
		Date:     time.Now().UTC().Format("2006-01-02"),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		NumNodes: numNodes,
		NumStubs: numStubs,
		NumRuns:  numRuns,
		P95:      measuredP95.Round(time.Microsecond).String(),
		Verdict:  v,
	}

	t := template.Must(template.New("results").Parse(resultsTmpl))
	f, err := os.Create(resultsPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", resultsPath, err)
	}
	defer f.Close()
	return t.Execute(f, data)
}
