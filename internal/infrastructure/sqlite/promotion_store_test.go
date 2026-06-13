package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/resolver"
)

func systemActor() domain.Actor {
	return domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
}

// TestPromotionStore_UnregisteredRepo verifies the registration check returns
// application.ErrUnregisteredRepo (type-assertable) for an unknown repo.
func TestPromotionStore_UnregisteredRepo(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	n := mustNode(t, "n1", "a.go", "A", domain.KindFunction)
	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "ghost", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{n}}},
	})
	var unreg application.ErrUnregisteredRepo
	if !errors.As(err, &unreg) {
		t.Fatalf("want ErrUnregisteredRepo, got %T: %v", err, err)
	}
	if unreg.RepoID != "ghost" {
		t.Errorf("RepoID = %q, want ghost", unreg.RepoID)
	}
}

// TestPromotionStore_RollsBackOnMidTxFailure proves the transaction is atomic:
// when a co-transactional write fails mid-promotion, every node/queue/FTS write
// from that Promote call is rolled back, leaving the prior committed state
// untouched.
func TestPromotionStore_RollsBackOnMidTxFailure(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// First promotion commits cleanly: 1 node.
	n1 := mustNode(t, "n1", "a.go", "A", domain.KindFunction)
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{n1}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	var nodes, queue int
	db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes)
	db.QueryRow(`SELECT COUNT(*) FROM post_promotion_queue`).Scan(&queue)
	if nodes != 1 {
		t.Fatalf("after first promote: nodes=%d want 1", nodes)
	}

	// Sabotage the FTS table so the next promotion fails mid-transaction,
	// AFTER the node rows for the file have been deleted+inserted.
	if _, err := db.Exec(`DROP TABLE node_fts_trigrams`); err != nil {
		t.Fatalf("drop fts table: %v", err)
	}

	// Second promotion: changes the node and adds a sibling. It must fail and
	// roll back completely.
	n1b := mustNode(t, "n1", "a.go", "A-changed", domain.KindFunction)
	n2 := mustNode(t, "n2", "a.go", "B", domain.KindFunction)
	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-2", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{n1b, n2}}},
	})
	if err == nil {
		t.Fatal("expected mid-tx failure, got nil")
		return
	}

	// The prior committed state must be intact: still exactly 1 node, the
	// original symbol, and the original queue rows — nothing from the failed
	// promotion leaked.
	var nodes2, queue2 int
	var symbol string
	db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes2)
	db.QueryRow(`SELECT COUNT(*) FROM post_promotion_queue`).Scan(&queue2)
	if err := db.QueryRow(`SELECT symbol_path FROM nodes WHERE node_id='n1'`).Scan(&symbol); err != nil {
		t.Fatalf("requery n1: %v", err)
	}
	if nodes2 != 1 {
		t.Errorf("nodes after rolled-back promote: want 1, got %d", nodes2)
	}
	if symbol != "A" {
		t.Errorf("symbol after rollback: want original %q, got %q", "A", symbol)
	}
	if queue2 != queue {
		t.Errorf("queue rows after rollback: want %d (unchanged), got %d", queue, queue2)
	}
}

// TestPromotionStore_CrossPackageCallsResolution pins solov2-xc51.2: a
// package-qualified call (cmd.Execute) in one package binds to the exported
// symbol in another package of the SAME module, emitting a concrete CALLS
// edge. main.go imports github.com/acme/app/cmd; the module_path on the repo
// row lets promotion map the import to the cmd/ package dir.
func TestPromotionStore_CrossPackageCallsResolution(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// cmd/root.go defines Execute; main.go calls cmd.Execute().
	exec := mustNode(t, "execID", "/tmp/app/cmd/root.go", "Execute", domain.KindFunction, domain.WithExported(true))
	mainFn := mustNode(t, "mainID", "/tmp/app/main.go", "main", domain.KindFunction)

	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "/tmp/app/cmd/root.go", Nodes: []*domain.Node{exec}},
			{
				Path:    "/tmp/app/main.go",
				Nodes:   []*domain.Node{mainFn},
				Imports: map[string]string{"cmd": "github.com/acme/app/cmd"},
				UnresolvedCalls: []domain.UnresolvedCall{
					{CallerID: "mainID", CalleeName: "Execute", PkgQualifier: "cmd"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='mainID' AND dst_node_id='execID'`,
	).Scan(&n); err != nil {
		t.Fatalf("query edge: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 cross-package CALLS edge main->Execute, got %d", n)
	}

	// An import outside the module must NOT produce a same-module CALLS edge.
	var stray int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='mainID' AND dst_node_id != 'execID'`).Scan(&stray)
	if stray != 0 {
		t.Errorf("unexpected stray CALLS edges from main: %d", stray)
	}
}

// TestPromotionStore_PersistsFileImports pins solov2-xjm5: imports parsed
// for each file land in file_imports with the (repo_id, branch, file_path)
// grain, and a re-promote of the same file replaces rather than duplicates.
func TestPromotionStore_PersistsFileImports(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	n1 := mustNode(t, "n1", "/tmp/app/cmd/root.go", "main", domain.KindFunction)

	// First promotion: 2 imports.
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{{
			Path: "/tmp/app/cmd/root.go", Nodes: []*domain.Node{n1},
			Imports: map[string]string{
				"cobra":    "github.com/spf13/cobra",
				"greetlib": "github.com/junior/greetlib",
			},
		}},
	}); err != nil {
		t.Fatalf("promote 1: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM file_imports WHERE repo_id='repo1' AND branch='main'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("after first promote: want 2 file_imports rows, got %d", n)
	}

	// Re-promote same file with the cobra import removed.
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha2", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{{
			Path: "/tmp/app/cmd/root.go", Nodes: []*domain.Node{n1},
			Imports: map[string]string{
				"greetlib": "github.com/junior/greetlib",
			},
		}},
	}); err != nil {
		t.Fatalf("promote 2: %v", err)
	}
	var have string
	if err := db.QueryRow(`SELECT GROUP_CONCAT(import_path, ',') FROM file_imports WHERE repo_id='repo1' AND branch='main'`).Scan(&have); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if have != "github.com/junior/greetlib" {
		t.Errorf("after re-promote with cobra removed: got %q, want %q", have, "github.com/junior/greetlib")
	}
}

// TestPromotionStore_ChainedSelectorMethodCallInModule covers solov2-9rc2
// Phase B: a chained-selector method call (`g := pkg.New(...); g.Hello()`)
// whose target package is in the SAME module must bind to the method node
// via bare-name lookup against `<Receiver>.<Method>`. Parser flags the
// UnresolvedCall with IsMethodCall=true; promotion resolves it by suffix
// match within the importing package's relDir.
func TestPromotionStore_ChainedSelectorMethodCallInModule(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// greet/greet.go defines Greeter.Hello (method) + New (constructor).
	// runner/runner.go has Run that does `g := greet.New(...); g.Hello(...)`.
	helloMethod := mustNode(t, "helloID", "/tmp/app/greet/greet.go", "Greeter.Hello", domain.KindMethod, domain.WithExported(true))
	newFn := mustNode(t, "newID", "/tmp/app/greet/greet.go", "New", domain.KindFunction, domain.WithExported(true))
	runFn := mustNode(t, "runID", "/tmp/app/runner/runner.go", "Run", domain.KindFunction, domain.WithExported(true))

	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "/tmp/app/greet/greet.go", Nodes: []*domain.Node{helloMethod, newFn}},
			{
				Path:    "/tmp/app/runner/runner.go",
				Nodes:   []*domain.Node{runFn},
				Imports: map[string]string{"greet": "github.com/acme/app/greet"},
				UnresolvedCalls: []domain.UnresolvedCall{
					// Plain pkg.New from `g := greet.New(...)`.
					{CallerID: "runID", CalleeName: "New", PkgQualifier: "greet"},
					// Chained-selector method call from `g.Hello(...)`.
					{CallerID: "runID", CalleeName: "Hello", PkgQualifier: "greet", IsMethodCall: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Phase B contract: Run -> Greeter.Hello edge must exist.
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID' AND dst_node_id='helloID'`,
	).Scan(&n); err != nil {
		t.Fatalf("query method edge: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 CALLS edge Run->Greeter.Hello (chained selector resolved), got %d", n)
	}
	// And the plain constructor call should also bind (regression guard for Phase A keying).
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID' AND dst_node_id='newID'`,
	).Scan(&n); err != nil {
		t.Fatalf("query plain edge: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 CALLS edge Run->New (plain pkg call), got %d", n)
	}
}

// TestPromotionStore_ChainedSelectorAmbiguityIsSkipped guards the
// no-false-edge invariant: if two receiver types in the target package own
// a method with the same name, the resolver must skip (not pick one
// arbitrarily). Phase C will surface this as a cross-repo stub once that
// path lands.
func TestPromotionStore_ChainedSelectorAmbiguityIsSkipped(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// Two distinct receiver types each owning a Hello method.
	helloA := mustNode(t, "helloAID", "/tmp/app/greet/greet.go", "TypeA.Hello", domain.KindMethod, domain.WithExported(true))
	helloB := mustNode(t, "helloBID", "/tmp/app/greet/greet.go", "TypeB.Hello", domain.KindMethod, domain.WithExported(true))
	runFn := mustNode(t, "runID", "/tmp/app/runner/runner.go", "Run", domain.KindFunction, domain.WithExported(true))

	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "/tmp/app/greet/greet.go", Nodes: []*domain.Node{helloA, helloB}},
			{
				Path:    "/tmp/app/runner/runner.go",
				Nodes:   []*domain.Node{runFn},
				Imports: map[string]string{"greet": "github.com/acme/app/greet"},
				UnresolvedCalls: []domain.UnresolvedCall{
					{CallerID: "runID", CalleeName: "Hello", PkgQualifier: "greet", IsMethodCall: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Neither edge should be emitted — ambiguity is skipped.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID'`).Scan(&n)
	if n != 0 {
		t.Errorf("want 0 CALLS edges from Run on ambiguous method (both helloA/helloB qualify); got %d", n)
	}
}

// TestPromotionStore_ChainedSelectorEmitsMethodCallStub covers solov2-9rc2
// Phase C: when a chained-selector method call (`g := pkg.New(...); g.X()`)
// imports from a DIFFERENT module (cross-repo), promotion must emit a
// cross_repo_edge_stub with method_call=1 and symbol_path=bare-method-name.
// The reverse resolver (phase D) binds this to the receiver method later.
func TestPromotionStore_ChainedSelectorEmitsMethodCallStub(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	runFn := mustNode(t, "runID", "/tmp/app/runner/runner.go", "Run", domain.KindFunction, domain.WithExported(true))

	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{
				Path:    "/tmp/app/runner/runner.go",
				Nodes:   []*domain.Node{runFn},
				Imports: map[string]string{"greetlib": "github.com/jrose/greetlib"},
				UnresolvedCalls: []domain.UnresolvedCall{
					// Plain pkg.New(...) — produces a regular stub (method_call=0).
					{CallerID: "runID", CalleeName: "New", PkgQualifier: "greetlib"},
					// g.Hello(...) chained selector — produces a method-call stub.
					{CallerID: "runID", CalleeName: "Hello", PkgQualifier: "greetlib", IsMethodCall: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Two stubs: plain (method_call=0, symbol_path=New) + method-call (method_call=1, symbol_path=Hello).
	rows, err := db.Query(`SELECT symbol_path, method_call FROM cross_repo_edge_stubs WHERE src_node_id = 'runID' ORDER BY symbol_path`)
	if err != nil {
		t.Fatalf("query stubs: %v", err)
	}
	defer rows.Close()
	type stubRow struct {
		symbolPath string
		methodCall int
	}
	var got []stubRow
	for rows.Next() {
		var s stubRow
		if err := rows.Scan(&s.symbolPath, &s.methodCall); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 stubs, got %d: %+v", len(got), got)
	}
	if got[0].symbolPath != "Hello" || got[0].methodCall != 1 {
		t.Errorf("method-call stub mismatch: %+v", got[0])
	}
	if got[1].symbolPath != "New" || got[1].methodCall != 0 {
		t.Errorf("plain stub mismatch: %+v", got[1])
	}
}

// TestPromotionStore_CrossPackageResolvesAgainstPromotedGraph pins the
// incremental-commit half of solov2-xc51.2: when the callee's file is NOT in
// the current batch (already promoted earlier), the qualified call still binds
// by falling back to a promoted-graph lookup.
func TestPromotionStore_CrossPackageResolvesAgainstPromotedGraph(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	ctx := context.Background()

	// First commit: only cmd/root.go.
	exec := mustNode(t, "execID", "/tmp/app/cmd/root.go", "Execute", domain.KindFunction, domain.WithExported(true))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "/tmp/app/cmd/root.go", Nodes: []*domain.Node{exec}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	// Second commit: only main.go (cmd/root.go unchanged, not in batch).
	mainFn := mustNode(t, "mainID", "/tmp/app/main.go", "main", domain.KindFunction)
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha2", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{{
			Path:            "/tmp/app/main.go",
			Nodes:           []*domain.Node{mainFn},
			Imports:         map[string]string{"cmd": "github.com/acme/app/cmd"},
			UnresolvedCalls: []domain.UnresolvedCall{{CallerID: "mainID", CalleeName: "Execute", PkgQualifier: "cmd"}},
		}},
	}); err != nil {
		t.Fatalf("second Promote: %v", err)
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='mainID' AND dst_node_id='execID'`).Scan(&n)
	if n != 1 {
		t.Errorf("want main->Execute resolved via promoted graph, got %d edges", n)
	}
}

// TestPromotionStore_IntraPackageResolvesAgainstPromotedGraph pins
// solov2-ll57.13: an incremental single-file commit whose plain (non-method,
// non-pkg-qualified) call targets a same-package symbol in an UNCHANGED sibling
// file (not in this batch) must still bind, by falling back to the promoted
// graph — the intra-package twin of CrossPackageResolvesAgainstPromotedGraph.
// Without the fallback the edge is silently dropped, which would flag the
// callee dead-code on a single-file save.
func TestPromotionStore_IntraPackageResolvesAgainstPromotedGraph(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	ctx := context.Background()

	// First commit: only util.go, defining helper.
	helper := mustNode(t, "helperID", "/tmp/app/util.go", "helper", domain.KindFunction)
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "/tmp/app/util.go", Nodes: []*domain.Node{helper}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	// Second commit: only main.go (util.go unchanged, not in batch). Run() makes
	// a bare-name call to helper — same package, cross-file, callee not staged.
	runFn := mustNode(t, "runID", "/tmp/app/main.go", "Run", domain.KindFunction)
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha2", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{{
			Path:            "/tmp/app/main.go",
			Nodes:           []*domain.Node{runFn},
			UnresolvedCalls: []domain.UnresolvedCall{{CallerID: "runID", CalleeName: "helper"}},
		}},
	}); err != nil {
		t.Fatalf("second Promote: %v", err)
	}

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID' AND dst_node_id='helperID'`).Scan(&n)
	if n != 1 {
		t.Errorf("want Run->helper resolved via promoted graph (intra-package fallback), got %d edges", n)
	}
}

// TestPromotionStore_CrossRepoStub pins solov2-xc51.3 / solov2-1gj: a
// package-qualified call into ANOTHER module records a cross_repo_edge_stub
// that the query-time resolver binds to the node in whichever registered repo
// owns that module_path. Stdlib calls (fmt.Println) record no stub.
func TestPromotionStore_CrossRepoStub(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	now := time.Now().UnixMilli()
	// Caller repo (the app) + the dependency repo (pflag), each with module_path.
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at, module_path, active_branch) VALUES (?,?,?,?,?)`,
		"app", "/tmp/app", now, "github.com/acme/app", "main"); err != nil {
		t.Fatalf("insert app repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at, module_path, active_branch) VALUES (?,?,?,?,?)`,
		"pflag", "/tmp/pflag", now, "github.com/spf13/pflag", "main"); err != nil {
		t.Fatalf("insert pflag repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	ctx := context.Background()

	// Promote the pflag dependency: it exports Parse.
	parse := mustNode(t, "parseID", "/tmp/pflag/flag.go", "Parse", domain.KindFunction, domain.WithLanguage("go"), domain.WithExported(true))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "pflag", Branch: "main", GitSHA: "p1", Actor: systemActor(), PromotedAt: now,
		Files: []application.PromotionFile{{Path: "/tmp/pflag/flag.go", Nodes: []*domain.Node{parse}}},
	}); err != nil {
		t.Fatalf("promote pflag: %v", err)
	}

	// Promote the app: main() calls flag.Parse() and fmt.Println().
	mainFn := mustNode(t, "mainID", "/tmp/app/main.go", "main", domain.KindFunction, domain.WithLanguage("go"))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "app", Branch: "main", GitSHA: "a1", Actor: systemActor(), PromotedAt: now,
		Files: []application.PromotionFile{{
			Path:  "/tmp/app/main.go",
			Nodes: []*domain.Node{mainFn},
			Imports: map[string]string{
				"flag": "github.com/spf13/pflag",
				"fmt":  "fmt",
			},
			UnresolvedCalls: []domain.UnresolvedCall{
				{CallerID: "mainID", CalleeName: "Parse", PkgQualifier: "flag"},
				{CallerID: "mainID", CalleeName: "Println", PkgQualifier: "fmt"},
			},
		}},
	}); err != nil {
		t.Fatalf("promote app: %v", err)
	}

	// One stub for flag.Parse; none for the stdlib fmt.Println.
	var nStub int
	db.QueryRow(`SELECT COUNT(*) FROM cross_repo_edge_stubs WHERE src_node_id='mainID'`).Scan(&nStub)
	if nStub != 1 {
		t.Fatalf("want exactly 1 cross-repo stub (flag.Parse), got %d", nStub)
	}
	var modulePath, symbol string
	db.QueryRow(`SELECT module_path, symbol_path FROM cross_repo_edge_stubs WHERE src_node_id='mainID'`).Scan(&modulePath, &symbol)
	if modulePath != "github.com/spf13/pflag" || symbol != "Parse" {
		t.Errorf("stub = (%q,%q), want (github.com/spf13/pflag, Parse)", modulePath, symbol)
	}

	// The query-time resolver binds the stub to pflag's Parse node.
	resolved, err := resolver.ResolveStubsForNode(ctx, db, "mainID", "main", false)
	if err != nil {
		t.Fatalf("resolve stubs: %v", err)
	}
	if len(resolved) != 1 || resolved[0].DstNodeID != "parseID" || resolved[0].DstRepoID != "pflag" {
		t.Errorf("resolved = %+v, want one edge -> parseID in repo pflag", resolved)
	}
}

// TestPromotionStore_RouteHandlerEdgeResolution pins solov2-ketg: a route
// node's ROUTES route→handler reference (UnresolvedCall{EdgeKind:
// EdgeRoutes}) binds to a same-package handler via the intra-package
// resolver and materialises a ROUTES edge — not a CALLS edge. This is the
// generalised resolver: buildCallEdge honours uc.EdgeKind.
func TestPromotionStore_RouteHandlerEdgeResolution(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// routes.go: a "GET /users" route plus its same-package handler in
	// another file of the package (handler.go).
	route := mustNode(t, "routeID", "/tmp/app/api/routes.go", "GET /users", domain.KindRoute)
	handler := mustNode(t, "handlerID", "/tmp/app/api/handler.go", "listUsers", domain.KindFunction)

	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "/tmp/app/api/handler.go", Nodes: []*domain.Node{handler}},
			{
				Path:  "/tmp/app/api/routes.go",
				Nodes: []*domain.Node{route},
				UnresolvedCalls: []domain.UnresolvedCall{
					{CallerID: "routeID", CalleeName: "listUsers", EdgeKind: domain.EdgeRoutes},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='ROUTES' AND src_node_id='routeID' AND dst_node_id='handlerID'`,
	).Scan(&n); err != nil {
		t.Fatalf("query route edge: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 ROUTES edge route->listUsers, got %d", n)
	}
	// The route→handler reference must NOT also leak a CALLS edge.
	var calls int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='routeID'`).Scan(&calls)
	if calls != 0 {
		t.Errorf("route→handler must not emit a CALLS edge, got %d", calls)
	}
}

// TestPromotionStore_RouteHandlerCrossRepoStub pins solov2-ketg: a route
// whose handler lives in another module records a cross-repo stub carrying
// kind='ROUTES' (not CALLS), so the query-time resolver materialises a
// ROUTES cross-repo edge. emitCrossRepoStub honours uc.EdgeKind and
// namespaces the stub_id by kind.
func TestPromotionStore_RouteHandlerCrossRepoStub(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "github.com/acme/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	route := mustNode(t, "routeID", "/tmp/app/api/routes.go", "GET /users", domain.KindRoute, domain.WithLanguage("go"))
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{{
			Path:    "/tmp/app/api/routes.go",
			Nodes:   []*domain.Node{route},
			Imports: map[string]string{"handlers": "github.com/acme/handlers"},
			UnresolvedCalls: []domain.UnresolvedCall{
				{CallerID: "routeID", CalleeName: "List", PkgQualifier: "handlers", EdgeKind: domain.EdgeRoutes},
			},
		}},
	}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var kind, symbol string
	if err := db.QueryRow(
		`SELECT kind, symbol_path FROM cross_repo_edge_stubs WHERE src_node_id='routeID'`,
	).Scan(&kind, &symbol); err != nil {
		t.Fatalf("query stub: %v", err)
	}
	if kind != "ROUTES" || symbol != "List" {
		t.Errorf("stub = (kind=%q, symbol=%q), want (ROUTES, List)", kind, symbol)
	}
}

// TestPromotionStore_EnqueuesExactlyOneWikiRow verifies AC1: a promotion
// enqueues exactly one repo-scoped WorkKindWiki row regardless of how many
// files the batch touches.
func TestPromotionStore_EnqueuesExactlyOneWikiRow(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// Multi-file batch — the wiki lane must still get exactly one row.
	na := mustNode(t, "na", "a.go", "A", domain.KindFunction)
	nb := mustNode(t, "nb", "b.go", "B", domain.KindFunction)
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "a.go", Nodes: []*domain.Node{na}},
			{Path: "b.go", Nodes: []*domain.Node{nb}},
		},
	}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var wikiRows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM post_promotion_queue WHERE work_kind='wiki'`,
	).Scan(&wikiRows); err != nil {
		t.Fatalf("count wiki rows: %v", err)
	}
	if wikiRows != 1 {
		t.Errorf("wiki rows = %d, want exactly 1", wikiRows)
	}

	// The wiki row carries an empty (repo-scoped) payload.
	var payload string
	if err := db.QueryRow(
		`SELECT payload FROM post_promotion_queue WHERE work_kind='wiki'`,
	).Scan(&payload); err != nil {
		t.Fatalf("read wiki payload: %v", err)
	}
	if payload != "" {
		t.Errorf("wiki payload = %q, want empty", payload)
	}
}

// TestPromotionStore_ReviewEnabled_EnqueuesPerFileReviewRow verifies AC1: with
// review enabled, a promotion enqueues a work_kind='review' row per changed
// file, payloaded with the file path.
func TestPromotionStore_ReviewEnabled_EnqueuesPerFileReviewRow(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(
		db,
		[]sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()},
		sqlite.WithReviewEnabled(true),
	)

	na := mustNode(t, "na", "a.go", "A", domain.KindFunction)
	nb := mustNode(t, "nb", "b.go", "B", domain.KindFunction)
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "a.go", Nodes: []*domain.Node{na}},
			{Path: "b.go", Nodes: []*domain.Node{nb}},
		},
	}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	rows, err := db.Query(
		`SELECT payload FROM post_promotion_queue WHERE work_kind='review' ORDER BY payload`)
	if err != nil {
		t.Fatalf("query review rows: %v", err)
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		payloads = append(payloads, p)
	}
	if got, want := payloads, []string{"a.go", "b.go"}; len(got) != len(want) ||
		got[0] != want[0] || got[1] != want[1] {
		t.Errorf("review payloads = %v, want %v (one per file)", got, want)
	}
}

// TestPromotionStore_ReviewDisabled_NoReviewRow verifies AC3: with review
// disabled (the default), no work_kind='review' row is enqueued.
func TestPromotionStore_ReviewDisabled_NoReviewRow(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	// Default construction: review disabled.
	store := sqlite.NewPromotionStore(
		db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	na := mustNode(t, "na", "a.go", "A", domain.KindFunction)
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{na}}},
	}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var reviewRows int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM post_promotion_queue WHERE work_kind='review'`,
	).Scan(&reviewRows); err != nil {
		t.Fatalf("count review rows: %v", err)
	}
	if reviewRows != 0 {
		t.Errorf("review rows = %d, want 0 when review disabled", reviewRows)
	}
}
