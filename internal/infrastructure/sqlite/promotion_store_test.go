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

	n, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
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
	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
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
	n1b, _ := domain.NewNode("n1", "a.go", "A-changed", domain.KindFunction)
	n2, _ := domain.NewNode("n2", "a.go", "B", domain.KindFunction)
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
	exec, _ := domain.NewNode("execID", "/tmp/app/cmd/root.go", "Execute", domain.KindFunction, domain.WithExported(true))
	mainFn, _ := domain.NewNode("mainID", "/tmp/app/main.go", "main", domain.KindFunction)

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
	exec, _ := domain.NewNode("execID", "/tmp/app/cmd/root.go", "Execute", domain.KindFunction, domain.WithExported(true))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "/tmp/app/cmd/root.go", Nodes: []*domain.Node{exec}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	// Second commit: only main.go (cmd/root.go unchanged, not in batch).
	mainFn, _ := domain.NewNode("mainID", "/tmp/app/main.go", "main", domain.KindFunction)
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
	parse, _ := domain.NewNode("parseID", "/tmp/pflag/flag.go", "Parse", domain.KindFunction, domain.WithLanguage("go"), domain.WithExported(true))
	if err := store.Promote(ctx, application.PromotionBatch{
		RepoID: "pflag", Branch: "main", GitSHA: "p1", Actor: systemActor(), PromotedAt: now,
		Files: []application.PromotionFile{{Path: "/tmp/pflag/flag.go", Nodes: []*domain.Node{parse}}},
	}); err != nil {
		t.Fatalf("promote pflag: %v", err)
	}

	// Promote the app: main() calls flag.Parse() and fmt.Println().
	mainFn, _ := domain.NewNode("mainID", "/tmp/app/main.go", "main", domain.KindFunction, domain.WithLanguage("go"))
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
	na, _ := domain.NewNode("na", "a.go", "A", domain.KindFunction)
	nb, _ := domain.NewNode("nb", "b.go", "B", domain.KindFunction)
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

	na, _ := domain.NewNode("na", "a.go", "A", domain.KindFunction)
	nb, _ := domain.NewNode("nb", "b.go", "B", domain.KindFunction)
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

	na, _ := domain.NewNode("na", "a.go", "A", domain.KindFunction)
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
