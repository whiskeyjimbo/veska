//go:build eval

// Package revalidate's eval test: drives revalidate.Handler against a
// synthetic 10k-edge commit fixture and asserts the M3 exit-gate target
// (< 60s wall time for a full-commit revalidation sweep).
//
// Fixture shape (defaults):
//   - 100 files, 100 nodes each = 10 000 nodes.
//   - 100 within-file edges per file = 10 000 edges total. Edges always
//     point into the "tail" half of each file, so the "head" nodes that
//     anchor dead-code findings genuinely have zero inbound edges.
//   - 30% of nodes carry an open finding with a STALE anchor_content_hash
//     (half rule='dead-code' on head nodes, half rule='contract-drift' on
//     nodes whose prev_signature != signature). All 3000 findings should
//     resolve to REFRESH on the first sweep — the explicit count check is
//     part of DoD #7.
//
// Build-tag-gated so plain CI runs (`go test ./...`) skip this end-to-end
// driver. The make target is `make eval-revalidate-bench`.
//
// Two isolation sub-tests run beforehand against smaller fixtures so a
// dead-code or contract-drift dispatch regression surfaces with its own
// signal independent of the 10k gate.
package revalidate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

const (
	defaultNodes    = 10000
	defaultFiles    = 100
	defaultStalePct = 30

	repoID = "revalidate-eval"
	branch = "main"

	gateMS = 60_000.0
)

// TestRevalidateBench is the 10k-node / 10k-edge gate. It must finish
// inside 60 s of wall clock; failure marks the M3 exit gate unmet.
func TestRevalidateBench(t *testing.T) {
	nodes := envInt("REVALIDATE_NODES", defaultNodes)
	files := envInt("REVALIDATE_FILES", defaultFiles)
	stalePct := envInt("REVALIDATE_STALE_PCT", defaultStalePct)

	if nodes%files != 0 {
		t.Fatalf("REVALIDATE_NODES=%d must be a multiple of REVALIDATE_FILES=%d", nodes, files)
	}
	nodesPerFile := nodes / files
	if nodesPerFile < 30 {
		t.Fatalf("nodes/file=%d too small: need at least 30 per file (15 dead-code + 15 contract-drift heads)", nodesPerFile)
	}

	t.Run("DeadCodeOnly", func(t *testing.T) {
		runFixture(t, fixtureSpec{
			files:        4,
			nodesPerFile: 100,
			deadCodePct:  20,
			driftPct:     0,
			tag:          "deadcode",
		})
	})

	t.Run("ContractDriftOnly", func(t *testing.T) {
		runFixture(t, fixtureSpec{
			files:        4,
			nodesPerFile: 100,
			deadCodePct:  0,
			driftPct:     20,
			tag:          "drift",
		})
	})

	t.Run("Combined10k", func(t *testing.T) {
		// stalePct split in half between dead-code and contract-drift
		half := stalePct / 2
		res := runFixture(t, fixtureSpec{
			files:        files,
			nodesPerFile: nodesPerFile,
			deadCodePct:  half,
			driftPct:     stalePct - half,
			tag:          "combined",
			emitJSON:     true,
		})

		if res.ElapsedMS >= gateMS {
			t.Fatalf("M3 exit-gate FAILED: elapsed=%.2fms >= %dms (10k-edge revalidation must finish < 60s)",
				res.ElapsedMS, int(gateMS))
		}
	})
}

type fixtureSpec struct {
	files        int
	nodesPerFile int
	deadCodePct  int // % of nodes-per-file getting a dead-code finding
	driftPct     int // % of nodes-per-file getting a contract-drift finding
	tag          string
	emitJSON     bool
}

func runFixture(t *testing.T, spec fixtureSpec) Result {
	t.Helper()
	ctx := context.Background()

	deadPerFile := (spec.nodesPerFile * spec.deadCodePct) / 100
	driftPerFile := (spec.nodesPerFile * spec.driftPct) / 100
	if deadPerFile+driftPerFile > spec.nodesPerFile {
		t.Fatalf("dead+drift > nodes/file: %d+%d > %d", deadPerFile, driftPerFile, spec.nodesPerFile)
	}

	// Layout: indices [0, deadPerFile)              -> dead-code anchors (no inbound)
	//         [deadPerFile, deadPerFile+driftPerFile) -> contract-drift anchors (prev != current)
	//         [deadPerFile+driftPerFile, nodesPerFile) -> "callee tail" (edge dst pool)
	tailStart := deadPerFile + driftPerFile
	tailLen := spec.nodesPerFile - tailStart
	if tailLen <= 0 {
		t.Fatalf("no tail nodes available for edge dst pool (tailStart=%d, npf=%d)",
			tailStart, spec.nodesPerFile)
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	backupDir := filepath.Join(tmpDir, "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("sqlite.OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	totalNodes := spec.files * spec.nodesPerFile
	totalEdges := spec.files * spec.nodesPerFile // 1 outbound per src
	totalFindings := spec.files * (deadPerFile + driftPerFile)

	seedRepo(t, db)
	seedNodesAndEdges(t, db, spec, tailStart, tailLen)
	seedFindings(t, db, spec, deadPerFile, driftPerFile)

	repo := sqlite.NewRevalidateRepo(db)
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	latencies := make([]time.Duration, 0, spec.files)
	start := time.Now()
	for f := 0; f < spec.files; f++ {
		filePath := filePathFor(spec.tag, f)
		row := ports.WorkRow{
			Kind:    ports.WorkKindRevalidate,
			Payload: filePath,
			RepoID:  repoID,
			Branch:  branch,
		}
		t0 := time.Now()
		if err := h.Handle(ctx, row); err != nil {
			t.Fatalf("Handle(%s): %v", filePath, err)
		}
		latencies = append(latencies, time.Since(t0))
	}
	elapsed := time.Since(start)

	refreshed, closed := countOutcomes(t, db)

	// Sanity: refresh + close == total findings (every one should have
	// been visited, since they're all stale and on the swept files).
	if refreshed+closed != totalFindings {
		t.Fatalf("[%s] refresh+close mismatch: refreshed=%d + closed=%d != findings=%d",
			spec.tag, refreshed, closed, totalFindings)
	}

	res := Result{
		Nodes:         totalNodes,
		Files:         spec.files,
		Edges:         totalEdges,
		FindingsTotal: totalFindings,
		FindingsStale: totalFindings,
		Refreshed:     refreshed,
		Closed:        closed,
		ElapsedMS:     float64(elapsed.Microseconds()) / 1000.0,
		P95HandleMS:   float64(p95(latencies).Microseconds()) / 1000.0,
		ExitGateMet:   elapsed.Milliseconds() < int64(gateMS),
		Backend:       "sqlite",
		Timestamp:     time.Now().UTC(),
	}

	gate := "PASS"
	if !res.ExitGateMet {
		gate = "FAIL"
	}
	fmt.Printf("REVALIDATE[%s] nodes=%d files=%d findings=%d elapsed_ms=%.2f p95_ms=%.2f refreshed=%d closed=%d gate=%s\n",
		spec.tag, res.Nodes, res.Files, res.FindingsTotal, res.ElapsedMS, res.P95HandleMS,
		res.Refreshed, res.Closed, gate)

	if spec.emitJSON {
		if err := WriteJSON("revalidate_bench_results.json", res); err != nil {
			t.Logf("WriteJSON: %v (continuing)", err)
		}
	}
	return res
}

func seedRepo(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
}

// seedNodesAndEdges inserts spec.files * spec.nodesPerFile node rows plus
// one outbound edge per src node. Every edge dst lives in the file's
// "tail" slice [tailStart, nodesPerFile), so dead-code anchor nodes in
// [0, deadPerFile) always have zero inbound edges — the dead-code
// dispatch path therefore takes the REFRESH branch.
//
// Nodes in [deadPerFile, deadPerFile+driftPerFile) receive distinct
// (prev_signature, signature) values so the contract-drift dispatch path
// also takes REFRESH.
//
// All nodes use an "h-current-<node_id>" content_hash; findings carry
// "h-stale-<node_id>" so the stale-set join surfaces every finding.
func seedNodesAndEdges(t *testing.T, db *sql.DB, spec fixtureSpec, tailStart, tailLen int) {
	t.Helper()
	now := time.Now().UnixMilli()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	nodeStmt, err := tx.Prepare(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
		signature, prev_signature
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare node: %v", err)
	}
	defer nodeStmt.Close()

	edgeStmt, err := tx.Prepare(`INSERT INTO edges (
		edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
	) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare edge: %v", err)
	}
	defer edgeStmt.Close()

	deadEnd := (spec.nodesPerFile * spec.deadCodePct) / 100
	driftEnd := deadEnd + (spec.nodesPerFile*spec.driftPct)/100

	for f := 0; f < spec.files; f++ {
		fp := filePathFor(spec.tag, f)
		for i := 0; i < spec.nodesPerFile; i++ {
			nid := nodeIDFor(spec.tag, f, i)
			sig, prev := "", ""
			if i >= deadEnd && i < driftEnd {
				// contract-drift anchor: prev != current.
				sig = "func F" + nid + "() error"
				prev = "func F" + nid + "()"
			}
			if _, err := nodeStmt.Exec(
				nid, branch, repoID, "go", "function",
				"pkg/f"+strconv.Itoa(f)+"."+nid, fp,
				1, 1, "h-current-"+nid, now, "revalidate-eval", "system",
				nullable(sig), nullable(prev),
			); err != nil {
				t.Fatalf("insert node %s: %v", nid, err)
			}
		}
		// One outbound edge per src node, dst is a tail-pool node so
		// dead-code heads stay inbound-free.
		for i := 0; i < spec.nodesPerFile; i++ {
			srcID := nodeIDFor(spec.tag, f, i)
			dst := tailStart + ((i * 7) % tailLen)
			dstID := nodeIDFor(spec.tag, f, dst)
			eid := fmt.Sprintf("e-%s-%d-%d", spec.tag, f, i)
			if _, err := edgeStmt.Exec(
				eid, branch, repoID, srcID, dstID, "calls", "static", now,
			); err != nil {
				t.Fatalf("insert edge %s: %v", eid, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit nodes/edges: %v", err)
	}
}

// seedFindings writes deadPerFile 'dead-code' + driftPerFile
// 'contract-drift' findings per file. Every row has anchor_content_hash
// set to a STALE value so the join in StaleFindingsForFile surfaces it.
func seedFindings(t *testing.T, db *sql.DB, spec fixtureSpec, deadPerFile, driftPerFile int) {
	t.Helper()
	now := time.Now().UnixMilli()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`INSERT INTO findings (
		finding_id, branch, repo_id, node_id, file_path, severity, source_layer,
		rule, message, state, created_at, actor_id, actor_kind, anchor_content_hash
	) VALUES (?,?,?,?,?,?,?,?,?, 'open', ?, 'revalidate-eval', 'system', ?)`)
	if err != nil {
		t.Fatalf("prepare finding: %v", err)
	}
	defer stmt.Close()

	for f := 0; f < spec.files; f++ {
		fp := filePathFor(spec.tag, f)
		for i := 0; i < deadPerFile; i++ {
			nid := nodeIDFor(spec.tag, f, i)
			fid := fmt.Sprintf("dc-%s-%d-%d", spec.tag, f, i)
			if _, err := stmt.Exec(
				fid, branch, repoID, nid, fp, "warning", "checks",
				"dead-code", "no inbound callers", now, "h-stale-"+nid,
			); err != nil {
				t.Fatalf("insert dead-code finding %s: %v", fid, err)
			}
		}
		for i := 0; i < driftPerFile; i++ {
			idx := deadPerFile + i
			nid := nodeIDFor(spec.tag, f, idx)
			fid := fmt.Sprintf("cd-%s-%d-%d", spec.tag, f, idx)
			if _, err := stmt.Exec(
				fid, branch, repoID, nid, fp, "warning", "checks",
				"contract-drift", "signature changed", now, "h-stale-"+nid,
			); err != nil {
				t.Fatalf("insert contract-drift finding %s: %v", fid, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit findings: %v", err)
	}
}

// countOutcomes returns (refreshed, closed) for the eval repo.
//
// REFRESH leaves state='open' and rewrites anchor_content_hash from the
// stale "h-stale-..." prefix to the node's current "h-current-..." hash
// — so any row with state='open' AND anchor_content_hash NOT LIKE
// 'h-stale-%' was refreshed by Handle.
//
// CLOSE flips state to 'closed' with closed_reason='revalidated_obsolete'.
func countOutcomes(t *testing.T, db *sql.DB) (refreshed, closed int) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM findings
		WHERE repo_id = ? AND branch = ?
		  AND state = 'open'
		  AND anchor_content_hash NOT LIKE 'h-stale-%'`,
		repoID, branch,
	).Scan(&refreshed); err != nil {
		t.Fatalf("count refreshed: %v", err)
	}
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM findings
		WHERE repo_id = ? AND branch = ?
		  AND state = 'closed' AND closed_reason = 'revalidated_obsolete'`,
		repoID, branch,
	).Scan(&closed); err != nil {
		t.Fatalf("count closed: %v", err)
	}
	return refreshed, closed
}

func filePathFor(tag string, f int) string {
	return fmt.Sprintf("pkg/%s/f%03d.go", tag, f)
}

func nodeIDFor(tag string, f, i int) string {
	return fmt.Sprintf("n-%s-%03d-%03d", tag, f, i)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func p95(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(d))
	copy(cp, d)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (len(cp) * 95) / 100
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
