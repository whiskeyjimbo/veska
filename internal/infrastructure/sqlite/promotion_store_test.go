// SPDX-License-Identifier: AGPL-3.0-only

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

	if _, err := db.Exec(`DROP TABLE node_fts_trigrams`); err != nil {
		t.Fatalf("drop fts table: %v", err)
	}

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

	var stray int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='mainID' AND dst_node_id != 'execID'`).Scan(&stray)
	if stray != 0 {
		t.Errorf("unexpected stray CALLS edges from main: %d", stray)
	}
}

// TestPromotionStore_FileImportsFlagOwnModule pins that a repo's own-module
// imports (which look third-party but live under the repo's module_path) are
// persisted flagged internal=1 - so the package-dependency aggregator can see
// them - while genuine external imports are stored internal=0 and remain the
// only entries in the external (deps-list) view.
func TestPromotionStore_FileImportsFlagOwnModule(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/modbeta", time.Now().UnixMilli(), "example.com/modbeta",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	n1 := mustNode(t, "n1", "/tmp/modbeta/main.go", "main", domain.KindFunction)

	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{{
			Path: "/tmp/modbeta/main.go", Nodes: []*domain.Node{n1},
			Imports: map[string]string{
				"widget": "example.com/modbeta/widget",  // own module - flagged internal=1
				"metric": "example.com/modalpha/metric", // genuine external dep - internal=0
			},
		}},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	flagOf := func(importPath string) int {
		var v int
		if err := db.QueryRow(
			`SELECT internal FROM file_imports WHERE repo_id='repo1' AND branch='main' AND import_path=?`,
			importPath,
		).Scan(&v); err != nil {
			t.Fatalf("scan internal flag for %q: %v", importPath, err)
		}
		return v
	}
	if got := flagOf("example.com/modbeta/widget"); got != 1 {
		t.Errorf("own-module import internal flag = %d, want 1", got)
	}
	if got := flagOf("example.com/modalpha/metric"); got != 0 {
		t.Errorf("external import internal flag = %d, want 0", got)
	}

	// The external (deps-list) view excludes the own-module import.
	var external string
	if err := db.QueryRow(
		`SELECT COALESCE(GROUP_CONCAT(import_path, ','), '') FROM file_imports WHERE repo_id='repo1' AND branch='main' AND internal=0`,
	).Scan(&external); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if external != "example.com/modalpha/metric" {
		t.Errorf("external view = %q, want only example.com/modalpha/metric", external)
	}
}

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
					{CallerID: "runID", CalleeName: "New", PkgQualifier: "greet"},
					{CallerID: "runID", CalleeName: "Hello", PkgQualifier: "greet", IsMethodCall: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID' AND dst_node_id='helloID'`,
	).Scan(&n); err != nil {
		t.Fatalf("query method edge: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 CALLS edge Run->Greeter.Hello (chained selector resolved), got %d", n)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID' AND dst_node_id='newID'`,
	).Scan(&n); err != nil {
		t.Fatalf("query plain edge: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 CALLS edge Run->New (plain pkg call), got %d", n)
	}
}

// TestPromotionStore_ChainedSelectorAmbiguityIsSkipped verifies that if multiple
// receiver types declare a method of the same name, the resolver skips the match
// to prevent emitting a false edge.
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

	var n int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='runID'`).Scan(&n)
	if n != 0 {
		t.Errorf("want 0 CALLS edges from Run on ambiguous method (both helloA/helloB qualify); got %d", n)
	}
}

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
				Imports: map[string]string{"greetlib": "github.com/example/greetlib"},
				UnresolvedCalls: []domain.UnresolvedCall{
					{CallerID: "runID", CalleeName: "New", PkgQualifier: "greetlib"},
					{CallerID: "runID", CalleeName: "Hello", PkgQualifier: "greetlib", IsMethodCall: true},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

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

	exec := mustNode(t, "execID", "/tmp/app/cmd/root.go", "Execute", domain.KindFunction, domain.WithExported(true))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "/tmp/app/cmd/root.go", Nodes: []*domain.Node{exec}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

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

	helper := mustNode(t, "helperID", "/tmp/app/util.go", "helper", domain.KindFunction)
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "/tmp/app/util.go", Nodes: []*domain.Node{helper}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

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

func TestPromotionStore_CrossRepoStub(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	now := time.Now().UnixMilli()
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

	parse := mustNode(t, "parseID", "/tmp/pflag/flag.go", "Parse", domain.KindFunction, domain.WithLanguage("go"), domain.WithExported(true))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "pflag", Branch: "main", GitSHA: "p1", Actor: systemActor(), PromotedAt: now,
		Files: []application.PromotionFile{{Path: "/tmp/pflag/flag.go", Nodes: []*domain.Node{parse}}},
	}); err != nil {
		t.Fatalf("promote pflag: %v", err)
	}

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

	resolved, err := resolver.ResolveStubsForNode(ctx, db, "mainID", "main", false)
	if err != nil {
		t.Fatalf("resolve stubs: %v", err)
	}
	if len(resolved) != 1 || resolved[0].DstNodeID != "parseID" || resolved[0].DstRepoID != "pflag" {
		t.Errorf("resolved = %+v, want one edge -> parseID in repo pflag", resolved)
	}
}

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
	var calls int
	db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind='CALLS' AND src_node_id='routeID'`).Scan(&calls)
	if calls != 0 {
		t.Errorf("route→handler must not emit a CALLS edge, got %d", calls)
	}
}

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

func TestPromotionStore_ReviewDisabled_NoReviewRow(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
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
