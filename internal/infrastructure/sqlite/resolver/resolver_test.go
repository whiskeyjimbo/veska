package resolver_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/resolver"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

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
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup stmt: %v\nSQL: %s", err, s)
		}
	}
	return db
}

func seedRepo(t *testing.T, db *sql.DB, repoID, modulePath, activeBranch string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, active_branch, module_path) VALUES (?, ?, ?, ?)`,
		repoID, "/path/to/"+repoID, activeBranch, modulePath,
	)
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
}

func seedNode(t *testing.T, db *sql.DB, nodeID, branch, repoID, language, kind, symbolPath, filePath string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO nodes (node_id, branch, repo_id, language, kind, symbol_path, file_path) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nodeID, branch, repoID, language, kind, symbolPath, filePath,
	)
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
}

func seedStub(t *testing.T, db *sql.DB, stubID, branch, repoID, srcNodeID, kind, modulePath, symbolPath, language string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO cross_repo_edge_stubs (stub_id, branch, repo_id, src_node_id, kind, module_path, symbol_path, language) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		stubID, branch, repoID, srcNodeID, kind, modulePath, symbolPath, language,
	)
	if err != nil {
		t.Fatalf("seed stub: %v", err)
	}
}

// TestResolveCrossRepoEdgeHit verifies a matching node is found and CrossRepo=true.
func TestResolveCrossRepoEdgeHit(t *testing.T) {
	db := setupTestDB(t)

	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	seedNode(t, db, "node-b-1", "main", "repo-b", "go", "func", "pkg.Foo", "file.go")

	stub := resolver.CrossRepoStub{
		StubID:     "stub-1",
		SrcNodeID:  "node-a-1",
		Kind:       "calls",
		ModulePath: "github.com/example/lib",
		SymbolPath: "pkg.Foo",
		Language:   "go",
	}

	edge, err := resolver.ResolveCrossRepoEdge(context.Background(), db, stub, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge == nil {
		t.Fatal("expected resolved edge, got nil")
		return
	}
	if edge.DstNodeID != "node-b-1" {
		t.Errorf("DstNodeID: got %q, want %q", edge.DstNodeID, "node-b-1")
	}
	if edge.DstRepoID != "repo-b" {
		t.Errorf("DstRepoID: got %q, want %q", edge.DstRepoID, "repo-b")
	}
	if edge.DstBranch != "main" {
		t.Errorf("DstBranch: got %q, want %q", edge.DstBranch, "main")
	}
	if edge.SrcNodeID != "node-a-1" {
		t.Errorf("SrcNodeID: got %q, want %q", edge.SrcNodeID, "node-a-1")
	}
	if edge.Kind != "calls" {
		t.Errorf("Kind: got %q, want %q", edge.Kind, "calls")
	}
	if !edge.CrossRepo {
		t.Error("CrossRepo should be true")
	}
}

// TestResolveCrossRepoEdgeMiss verifies a silent nil,nil when no node matches.
func TestResolveCrossRepoEdgeMiss(t *testing.T) {
	db := setupTestDB(t)

	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	// No node seeded — intentional miss.

	stub := resolver.CrossRepoStub{
		StubID:     "stub-miss",
		SrcNodeID:  "node-a-1",
		Kind:       "calls",
		ModulePath: "github.com/example/lib",
		SymbolPath: "pkg.Bar",
		Language:   "go",
	}

	edge, err := resolver.ResolveCrossRepoEdge(context.Background(), db, stub, false)
	if err != nil {
		t.Fatalf("expected nil error on miss, got: %v", err)
	}
	if edge != nil {
		t.Errorf("expected nil edge on miss, got: %+v", edge)
	}
}

// TestResolveStubsForNode verifies that two stubs for a node yield one resolved edge (one miss).
func TestResolveStubsForNode(t *testing.T) {
	db := setupTestDB(t)

	seedRepo(t, db, "repo-src", "github.com/example/src", "main")
	seedNode(t, db, "node-src-1", "main", "repo-src", "go", "func", "pkg.Caller", "caller.go")

	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	seedNode(t, db, "node-b-1", "main", "repo-b", "go", "func", "pkg.Foo", "foo.go")

	// Stub 1: resolvable
	seedStub(t, db, "stub-1", "main", "repo-src", "node-src-1", "calls", "github.com/example/lib", "pkg.Foo", "go")
	// Stub 2: miss (symbol doesn't exist)
	seedStub(t, db, "stub-2", "main", "repo-src", "node-src-1", "calls", "github.com/example/lib", "pkg.Ghost", "go")

	edges, err := resolver.ResolveStubsForNode(context.Background(), db, "node-src-1", "main", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 resolved edge, got %d", len(edges))
	}
	if edges[0].DstNodeID != "node-b-1" {
		t.Errorf("DstNodeID: got %q, want %q", edges[0].DstNodeID, "node-b-1")
	}
	if !edges[0].CrossRepo {
		t.Error("CrossRepo should be true")
	}
}

// TestResolveStubsExpandFalse verifies expand=true and expand=false produce identical single-hop results.
func TestResolveStubsExpandFalse(t *testing.T) {
	db := setupTestDB(t)

	seedRepo(t, db, "repo-src", "github.com/example/src", "main")
	seedNode(t, db, "node-src-1", "main", "repo-src", "go", "func", "pkg.Caller", "caller.go")

	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	seedNode(t, db, "node-b-1", "main", "repo-b", "go", "func", "pkg.Foo", "foo.go")

	seedStub(t, db, "stub-1", "main", "repo-src", "node-src-1", "calls", "github.com/example/lib", "pkg.Foo", "go")

	edgesDefault, err := resolver.ResolveStubsForNode(context.Background(), db, "node-src-1", "main", false)
	if err != nil {
		t.Fatalf("expand=false error: %v", err)
	}
	edgesExpand, err := resolver.ResolveStubsForNode(context.Background(), db, "node-src-1", "main", true)
	if err != nil {
		t.Fatalf("expand=true error: %v", err)
	}

	if len(edgesDefault) != len(edgesExpand) {
		t.Errorf("expected same count: default=%d expand=%d", len(edgesDefault), len(edgesExpand))
	}
	if len(edgesDefault) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edgesDefault))
	}
	d, e := edgesDefault[0], edgesExpand[0]
	if d.DstNodeID != e.DstNodeID || d.CrossRepo != e.CrossRepo || d.Kind != e.Kind {
		t.Errorf("mismatch: default=%+v expand=%+v", d, e)
	}
}
