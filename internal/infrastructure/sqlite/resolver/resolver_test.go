// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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
			method_call      INTEGER NOT NULL DEFAULT 0,
			src_line         INTEGER,
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

// seedMethodStub inserts a method-call cross-repo stub.
// symbolPath is the bare method name (e.g. "Hello"); the resolver matches
// it against `<Receiver>.Hello` in the target package.
func seedMethodStub(t *testing.T, db *sql.DB, stubID, branch, repoID, srcNodeID, modulePath, methodName string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO cross_repo_edge_stubs (stub_id, branch, repo_id, src_node_id, kind, module_path, symbol_path, language, method_call) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		stubID, branch, repoID, srcNodeID, "CALLS", modulePath, methodName, "go",
	)
	if err != nil {
		t.Fatalf("seed method stub: %v", err)
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
	// No node seeded - intentional miss.

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

// TestResolveCrossRepoEdge_Subpackage pins: an import path that is
// a subpackage of a registered multi-package module resolves via longest
// prefix-match (e.g. github.com/spf13/cobra/doc binds to the cobra repo when
// only github.com/spf13/cobra is registered) and the symbol lookup is
// constrained to the subpackage directory.
func TestResolveCrossRepoEdge_Subpackage(t *testing.T) {
	db := setupTestDB(t)

	// Single repo whose module root covers multiple packages.
	seedRepo(t, db, "repo-cobra", "github.com/spf13/cobra", "main")
	// Same-named symbol exists in TWO subpackages - only the right one must bind.
	seedNode(t, db, "node-doc", "main", "repo-cobra", "go", "function", "Render", "doc/util.go")
	seedNode(t, db, "node-root", "main", "repo-cobra", "go", "function", "Render", "command.go")

	stub := resolver.CrossRepoStub{
		StubID:     "stub-sub",
		SrcNodeID:  "caller",
		Kind:       "CALLS",
		ModulePath: "github.com/spf13/cobra/doc",
		SymbolPath: "Render",
		Language:   "go",
	}
	edge, err := resolver.ResolveCrossRepoEdge(context.Background(), db, stub, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge == nil {
		t.Fatal("expected resolved edge for subpackage import, got nil")
		return
	}
	if edge.DstNodeID != "node-doc" {
		t.Errorf("DstNodeID = %q, want node-doc (the doc-subpackage Render, not the root-package one)", edge.DstNodeID)
	}
}

// TestResolveCrossRepoEdge_LongestPrefixWins pins the disambiguation rule: when
// two registered repos could prefix-match (one is a parent module of the other),
// the more-specific repo binds.
func TestResolveCrossRepoEdge_LongestPrefixWins(t *testing.T) {
	db := setupTestDB(t)

	// Two nested module roots; the more-specific (sub) repo is the right target.
	seedRepo(t, db, "repo-parent", "github.com/acme/parent", "main")
	seedRepo(t, db, "repo-sub", "github.com/acme/parent/sub", "main")
	seedNode(t, db, "node-parent", "main", "repo-parent", "go", "function", "Foo", "sub/x.go")
	seedNode(t, db, "node-sub", "main", "repo-sub", "go", "function", "Foo", "x.go")

	stub := resolver.CrossRepoStub{
		StubID:     "stub-lpw",
		SrcNodeID:  "caller",
		Kind:       "CALLS",
		ModulePath: "github.com/acme/parent/sub",
		SymbolPath: "Foo",
		Language:   "go",
	}
	edge, err := resolver.ResolveCrossRepoEdge(context.Background(), db, stub, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge == nil {
		t.Fatal("expected resolved edge from longest-prefix repo, got nil")
		return
	}
	if edge.DstRepoID != "repo-sub" {
		t.Errorf("DstRepoID = %q, want repo-sub (more specific module_path)", edge.DstRepoID)
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

// TestResolveCrossRepoEdge_MethodCallSuffixMatch covers
// forward path: a stub flagged MethodCall=true with bare symbol_path "Hello"
// resolves to a method node whose symbol_path is "Greeter.Hello" via
// suffix match in the destination subpackage.
func TestResolveCrossRepoEdge_MethodCallSuffixMatch(t *testing.T) {
	db := setupTestDB(t)
	seedRepo(t, db, "repo-lib", "github.com/example/lib", "main")
	seedNode(t, db, "lib-greeter-hello", "main", "repo-lib", "go", "method", "Greeter.Hello", "file.go")

	stub := resolver.CrossRepoStub{
		StubID:     "stub-method",
		SrcNodeID:  "app-run",
		Kind:       "CALLS",
		ModulePath: "github.com/example/lib",
		SymbolPath: "Hello", // bare method name from chained-selector emission
		Language:   "go",
		MethodCall: true,
	}
	edge, err := resolver.ResolveCrossRepoEdge(context.Background(), db, stub, false)
	if err != nil {
		t.Fatalf("ResolveCrossRepoEdge: %v", err)
	}
	if edge == nil {
		t.Fatal("expected method-call stub to resolve to Greeter.Hello, got nil")
	}
	if edge.DstNodeID != "lib-greeter-hello" {
		t.Errorf("DstNodeID = %q, want lib-greeter-hello", edge.DstNodeID)
	}
}

// TestResolveCrossRepoEdge_MethodCallAmbiguousIsDropped guards the
// no-false-edges invariant for the cross-repo path: when two receiver
// types in the destination subpackage share a method name, the stub
// resolves to nothing rather than picking one arbitrarily.
func TestResolveCrossRepoEdge_MethodCallAmbiguousIsDropped(t *testing.T) {
	db := setupTestDB(t)
	seedRepo(t, db, "repo-lib", "github.com/example/lib", "main")
	seedNode(t, db, "lib-a-hello", "main", "repo-lib", "go", "method", "TypeA.Hello", "file.go")
	seedNode(t, db, "lib-b-hello", "main", "repo-lib", "go", "method", "TypeB.Hello", "file.go")

	stub := resolver.CrossRepoStub{
		StubID:     "stub-ambig",
		SrcNodeID:  "app-run",
		Kind:       "CALLS",
		ModulePath: "github.com/example/lib",
		SymbolPath: "Hello",
		Language:   "go",
		MethodCall: true,
	}
	edge, err := resolver.ResolveCrossRepoEdge(context.Background(), db, stub, false)
	if err != nil {
		t.Fatalf("ResolveCrossRepoEdge: %v", err)
	}
	if edge != nil {
		t.Errorf("ambiguous method-call stub must not resolve; got %+v", edge)
	}
}

// TestResolveStubsTargetingNode_MatchesMethodCallStub covers
// phase D reverse path: a method node Greeter.Hello in repo-lib must
// surface as the dst of a method-call stub from repo-app whose
// symbol_path is the bare "Hello".
func TestResolveStubsTargetingNode_MatchesMethodCallStub(t *testing.T) {
	db := setupTestDB(t)
	// Library repo with a method.
	seedRepo(t, db, "repo-lib", "github.com/example/lib", "main")
	seedNode(t, db, "lib-greeter-hello", "main", "repo-lib", "go", "method", "Greeter.Hello", "file.go")
	// Consumer repo with a chained-selector method-call stub.
	seedRepo(t, db, "repo-app", "github.com/example/app", "main")
	seedNode(t, db, "app-run", "main", "repo-app", "go", "function", "Run", "main.go")
	seedMethodStub(t, db, "stub-method-1", "main", "repo-app", "app-run",
		"github.com/example/lib", "Hello")

	edges, err := resolver.ResolveStubsTargetingNode(context.Background(), db, "lib-greeter-hello", "main")
	if err != nil {
		t.Fatalf("ResolveStubsTargetingNode: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("want 1 inbound edge from method-call stub, got %d: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.SrcNodeID != "app-run" || e.DstNodeID != "lib-greeter-hello" || !e.CrossRepo {
		t.Errorf("inbound method-call edge mismatch: %+v", e)
	}
}

// TestResolveStubsTargetingNode_FindsInboundCallers covers the
// happy path: a library symbol N in repo-b has a stub in repo-a pointing
// at it. The reverse resolver must surface that stub as an inbound edge
// (DstNodeID == N), enabling blast_radius/call_chain on a library symbol
// to find consumers in other repos.
func TestResolveStubsTargetingNode_FindsInboundCallers(t *testing.T) {
	db := setupTestDB(t)

	// Library repo with one exported symbol.
	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	seedNode(t, db, "node-b-target", "main", "repo-b", "go", "func", "pkg.Foo", "file.go")

	// Consumer repo with a stub pointing at lib.pkg.Foo.
	seedRepo(t, db, "repo-a", "github.com/example/app", "main")
	seedNode(t, db, "node-a-caller", "main", "repo-a", "go", "func", "main.Run", "main.go")
	seedStub(t, db, "stub-1", "main", "repo-a", "node-a-caller", "calls",
		"github.com/example/lib", "pkg.Foo", "go")

	edges, err := resolver.ResolveStubsTargetingNode(context.Background(), db, "node-b-target", "main")
	if err != nil {
		t.Fatalf("ResolveStubsTargetingNode: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("want 1 inbound edge, got %d: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.SrcNodeID != "node-a-caller" || e.DstNodeID != "node-b-target" || e.DstRepoID != "repo-b" || e.Kind != "calls" || !e.CrossRepo {
		t.Errorf("inbound edge mismatch: %+v", e)
	}
}

// TestResolveStubsTargetingNode_ExcludesIntraRepoStubs guards against
// returning a stub whose src repo equals the dst node's repo - that's
// not a cross-repo edge by definition and would leak self-references.
func TestResolveStubsTargetingNode_ExcludesIntraRepoStubs(t *testing.T) {
	db := setupTestDB(t)
	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	seedNode(t, db, "node-b-target", "main", "repo-b", "go", "func", "pkg.Foo", "file.go")
	seedNode(t, db, "node-b-caller", "main", "repo-b", "go", "func", "pkg.Caller", "caller.go")
	// Same-repo stub - must NOT be returned.
	seedStub(t, db, "stub-self", "main", "repo-b", "node-b-caller", "calls",
		"github.com/example/lib", "pkg.Foo", "go")

	edges, err := resolver.ResolveStubsTargetingNode(context.Background(), db, "node-b-target", "main")
	if err != nil {
		t.Fatalf("ResolveStubsTargetingNode: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("want 0 edges (intra-repo stub excluded), got %d: %+v", len(edges), edges)
	}
}

// TestResolveStubsTargetingNode_NoStubs returns empty when nothing points
// at the target - the common case for a fresh leaf symbol.
func TestResolveStubsTargetingNode_NoStubs(t *testing.T) {
	db := setupTestDB(t)
	seedRepo(t, db, "repo-b", "github.com/example/lib", "main")
	seedNode(t, db, "node-b-target", "main", "repo-b", "go", "func", "pkg.Foo", "file.go")

	edges, err := resolver.ResolveStubsTargetingNode(context.Background(), db, "node-b-target", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("want 0 edges, got %d", len(edges))
	}
}

// TestResolveStubsTargetingNode_UnknownNode returns empty silently when
// the dst node_id doesn't exist - a resolver call from a stale plan must
// not error the entire blast response.
func TestResolveStubsTargetingNode_UnknownNode(t *testing.T) {
	db := setupTestDB(t)
	edges, err := resolver.ResolveStubsTargetingNode(context.Background(), db, "node-missing", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("want 0 edges for unknown node, got %d", len(edges))
	}
}
