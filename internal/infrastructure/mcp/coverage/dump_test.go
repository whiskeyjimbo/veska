package coverage

// This is the THROWAWAY exploratory dump used to AUTHOR the frozen manifest
// It is NOT a coverage harness and asserts nothing about tool
// behaviour. It indexes the two fixture modules through the real cold-scan
// pipeline (no Ollama) and logs every (path,kind,name) node, every edge
// resolved back to its endpoint keys, every cross-repo stub, and every TODO.
// Run it with:
//	go test./internal/infrastructure/mcp/coverage/ -run TestDumpFixtureFacts -v
// then read the t.Log output and transcribe the facts into manifest.go. It is
// gated behind TestDumpFixtureFacts (a normal test name) but does no
// assertions, so it stays green in CI while remaining a one-shot authoring aid.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

func TestDumpFixtureFacts(t *testing.T) {
	db := indexFixtures(t)

	dumpNodes(t, db)
	dumpEdges(t, db)
	dumpStubs(t, db)
	dumpFileImports(t, db)
	dumpTodos(t, db)
}

func dumpTodos(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT repo_id, file_path, rule, message FROM findings WHERE rule = 'todo' ORDER BY repo_id, file_path`)
	if err != nil {
		t.Fatalf("query todo findings: %v", err)
	}
	defer rows.Close()
	t.Log("=== TODO FINDINGS (repo | file_path | rule | message) ===")
	for rows.Next() {
		var repo, fp, rule, msg string
		if err := rows.Scan(&repo, &fp, &rule, &msg); err != nil {
			t.Fatalf("scan todo: %v", err)
		}
		t.Logf("TODO %s | %s | %s | %q", repo, fp, rule, msg)
	}
}

func dumpNodes(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT repo_id, kind, file_path, symbol_path FROM nodes ORDER BY repo_id, file_path, kind, symbol_path`)
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	defer rows.Close()
	t.Log("=== NODES (repo | kind | file_path | symbol_path) ===")
	for rows.Next() {
		var repo, kind, fp, sp string
		if err := rows.Scan(&repo, &kind, &fp, &sp); err != nil {
			t.Fatalf("scan node: %v", err)
		}
		t.Logf("NODE %s | %s | %s | %s", repo, kind, fp, sp)
	}
}

func dumpEdges(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`
		SELECT e.repo_id, e.kind,
		       s.kind, s.file_path, s.symbol_path,
		       d.kind, d.file_path, d.symbol_path
		FROM edges e
		JOIN nodes s ON s.node_id = e.src_node_id AND s.branch = e.branch
		JOIN nodes d ON d.node_id = e.dst_node_id AND d.branch = e.branch
		ORDER BY e.repo_id, e.kind, s.symbol_path, d.symbol_path`)
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	defer rows.Close()
	t.Log("=== EDGES (repo | edgeKind | srcKind:srcFile:srcSym -> dstKind:dstFile:dstSym) ===")
	for rows.Next() {
		var repo, ek, sk, sf, ss, dk, df, ds string
		if err := rows.Scan(&repo, &ek, &sk, &sf, &ss, &dk, &df, &ds); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		t.Logf("EDGE %s | %s | %s:%s:%s -> %s:%s:%s", repo, ek, sk, sf, ss, dk, df, ds)
	}
}

func dumpStubs(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`
		SELECT st.repo_id, st.kind, st.module_path, st.symbol_path,
		       s.kind, s.file_path, s.symbol_path
		FROM cross_repo_edge_stubs st
		JOIN nodes s ON s.node_id = st.src_node_id AND s.branch = st.branch
		ORDER BY st.repo_id, st.module_path, st.symbol_path`)
	if err != nil {
		t.Fatalf("query stubs: %v", err)
	}
	defer rows.Close()
	t.Log("=== CROSS-REPO STUBS (repo | kind | module_path | symbol_path | srcKind:srcFile:srcSym) ===")
	for rows.Next() {
		var repo, k, mp, sp, sk, sf, ss string
		if err := rows.Scan(&repo, &k, &mp, &sp, &sk, &sf, &ss); err != nil {
			t.Fatalf("scan stub: %v", err)
		}
		t.Logf("STUB %s | %s | %s | %s | %s:%s:%s", repo, k, mp, sp, sk, sf, ss)
	}
}

func dumpFileImports(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT repo_id, file_path, import_path FROM file_imports ORDER BY repo_id, file_path, import_path`)
	if err != nil {
		t.Logf("query file_imports (may not exist): %v", err)
		return
	}
	defer rows.Close()
	t.Log("=== FILE IMPORTS (repo | file_path | import_path) ===")
	for rows.Next() {
		var repo, fp, ip string
		if err := rows.Scan(&repo, &fp, &ip); err != nil {
			t.Fatalf("scan file_import: %v", err)
		}
		t.Logf("IMPORT %s | %s | %s", repo, fp, ip)
	}
}

// indexFixtures cold-scans both fixture modules into a fresh in-memory-ish
// sqlite DB (temp file) as two separate repos with their real module paths,
// so modbeta's call into modalpha lands as a cross-repo stub. Shared by the
// dump and the frozen self-test.
func indexFixtures(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "coverage.sqlite")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	indexOne(t, db, AlphaRepoID, "example.com/modalpha", testdataDir(t, "modalpha"))
	indexOne(t, db, BetaRepoID, "example.com/modbeta", testdataDir(t, "modbeta"))
	return db
}

func indexOne(t *testing.T, db *sql.DB, repoID, modulePath, root string) {
	t.Helper()

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path) VALUES (?, ?, ?, ?, ?)`,
		repoID, root, time.Now().UnixMilli(), FixtureBranch, modulePath,
	); err != nil {
		t.Fatalf("insert repos row (%s): %v", repoID, err)
	}

	parser := treesitter.NewGoParser()
	area := staging.NewArea()
	gate := staging.NewGate(area)
	ingester := application.NewIngester(parser, area, gate,
		application.WithFindingStorage(sqlite.NewFindingRepo(db)))
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	promoter := application.NewPromoter(area, store)

	reparser, err := application.NewColdScanReparser(
		ingester, promoter, fixtureGit{head: "sha-" + repoID},
	)
	if err != nil {
		t.Fatalf("NewColdScanReparser: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := reparser(ctx, application.RepoRecord{
		RepoID:       repoID,
		RootPath:     root,
		ActiveBranch: FixtureBranch,
	}); err != nil {
		t.Fatalf("reparser (%s): %v", repoID, err)
	}
}

// testdataDir returns the absolute path to a fixture module under testdata/.
func testdataDir(t *testing.T, mod string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", mod))
	if err != nil {
		t.Fatalf("abs testdata/%s: %v", mod, err)
	}
	return abs
}

// fixtureGit is a stub headQuerier for the cold-scan pipeline; the fixture is
// not a real git repo so HEAD is fixed.
type fixtureGit struct{ head string }

func (f fixtureGit) HEAD(string) (string, error) { return f.head, nil }
