package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// providersTestDB builds an in-memory SQLite with the minimal tables the
// admin providers query: repos, node_embedding_refs, schema_migrations.
func providersTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`CREATE TABLE repos (
			repo_id           TEXT PRIMARY KEY,
			root_path         TEXT NOT NULL UNIQUE,
			added_at          INTEGER NOT NULL,
			active_branch     TEXT,
			last_promoted_sha TEXT,
			module_path       TEXT,
			kind              TEXT NOT NULL DEFAULT 'tracked',
			canonical_url     TEXT,
			last_accessed_at  INTEGER,
			prompted_at       INTEGER
		)`,
		`CREATE TABLE node_embedding_refs (
			node_id     TEXT PRIMARY KEY,
			state       TEXT NOT NULL,
			enqueued_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE schema_migrations (
			version       INTEGER PRIMARY KEY,
			applied_at    INTEGER,
			binary_sha    TEXT,
			migration_sha TEXT,
			applied_by    TEXT
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func TestRepoLister_MapsRecords(t *testing.T) {
	db := providersTestDB(t)
	_, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, last_promoted_sha)
		 VALUES (?,?,?,?,?), (?,?,?,?,?)`,
		"r1", "/path/one", 1, "main", "sha1",
		"r2", "/path/two", 2, "dev", "sha2",
	)
	if err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	rl := &repoLister{db: db}
	got, err := rl.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	if got[0].RepoID != "r1" || got[0].RootPath != "/path/one" ||
		got[0].ActiveBranch != "main" || got[0].LastPromotedSHA != "sha1" {
		t.Errorf("record 0 mismatch: %+v", got[0])
	}
	if got[1].RepoID != "r2" || got[1].RootPath != "/path/two" ||
		got[1].ActiveBranch != "dev" || got[1].LastPromotedSHA != "sha2" {
		t.Errorf("record 1 mismatch: %+v", got[1])
	}
}

func TestStatusProvider_ReportsDBState(t *testing.T) {
	db := providersTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES ('r1','/a',1), ('r2','/b',2)`,
	); err != nil {
		t.Fatalf("seed repos: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_embedding_refs (node_id, state) VALUES
			('n1','pending'), ('n2','pending'), ('n3','ready')`,
	); err != nil {
		t.Fatalf("seed refs: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_migrations (version) VALUES (1), (2), (3)`,
	); err != nil {
		t.Fatalf("seed migrations: %v", err)
	}

	sp := &statusProvider{db: db}
	m, err := sp.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// solov2-30sa: a non-zero pending_embeds count flips status to degraded
	// and adds embeddings_pending to degraded_reasons (matches what
	// eng_search_semantic already emits for the same backlog).
	if m["status"] != "degraded" {
		t.Errorf("status = %v; want degraded (pending_embeds > 0)", m["status"])
	}
	if m["schema_version"] != 3 {
		t.Errorf("schema_version = %v; want 3", m["schema_version"])
	}
	if m["repo_count"] != 2 {
		t.Errorf("repo_count = %v; want 2", m["repo_count"])
	}
	if m["pending_embeds"] != 2 {
		t.Errorf("pending_embeds = %v; want 2", m["pending_embeds"])
	}
	reasons, ok := m["degraded_reasons"].([]string)
	if !ok {
		t.Fatalf("degraded_reasons missing or wrong type: %v", m["degraded_reasons"])
	}
	if len(reasons) != 1 || reasons[0] != mcp.DegradedReasonEmbeddingsPending {
		t.Errorf("degraded_reasons = %v; want [embeddings_pending]", reasons)
	}
}

// TestStatusProvider_HealthyWhenNoPending pins the matched-pair: with zero
// pending embeds, status stays "ok" and degraded_reasons is empty (solov2-30sa).
func TestStatusProvider_HealthyWhenNoPending(t *testing.T) {
	db := providersTestDB(t)
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at, active_branch) VALUES ('r1', '/r1', 1, 'main')`); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (1)`); err != nil {
		t.Fatalf("seed migrations: %v", err)
	}
	sp := &statusProvider{db: db}
	m, err := sp.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v; want ok (no pending embeds)", m["status"])
	}
	if reasons, _ := m["degraded_reasons"].([]string); len(reasons) != 0 {
		t.Errorf("degraded_reasons = %v; want []", reasons)
	}
}

// TestStatusProvider_BacklogStaysDegradedForAgentContract pins solov2-34rl:
// even though `doctor status` no longer escalates a non-zero backlog to
// "degraded" (it now reports the backlog as a separate informational line),
// `eng_get_status` MUST keep reporting `degraded_reasons:[embeddings_pending]`
// while pending_embeds > 0. Agents rely on that signal to choose between the
// semantic and lexical search paths during the indexing-lag window — that
// contract predates the doctor reconciliation and must not regress.
func TestStatusProvider_BacklogStaysDegradedForAgentContract(t *testing.T) {
	db := providersTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES ('r1','/a',1)`,
	); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_embedding_refs (node_id, state) VALUES ('n1','pending')`,
	); err != nil {
		t.Fatalf("seed refs: %v", err)
	}
	sp := &statusProvider{db: db}
	m, err := sp.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if m["status"] != "degraded" {
		t.Fatalf("eng_get_status rollup = %v; want degraded (agent contract: see solov2-34rl)", m["status"])
	}
	reasons, _ := m["degraded_reasons"].([]string)
	if !slices.Contains(reasons, mcp.DegradedReasonEmbeddingsPending) {
		t.Errorf("eng_get_status degraded_reasons = %v; must include embeddings_pending (agents pick semantic-vs-lexical from this)", reasons)
	}
	if m["pending_embeds"] != 1 {
		t.Errorf("pending_embeds = %v; want 1", m["pending_embeds"])
	}
}

func TestStatusProvider_EmptySchemaTreatedAsZero(t *testing.T) {
	db := providersTestDB(t) // schema_migrations empty -> MAX is NULL
	sp := &statusProvider{db: db}
	m, err := sp.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if m["schema_version"] != 0 {
		t.Errorf("schema_version = %v; want 0", m["schema_version"])
	}
}

func TestConfigProvider_ReflectsConfig(t *testing.T) {
	cfg := Config{
		VeskaHome:     "/home/v",
		SQLitePath:    "/home/v/veska.db",
		CLISockPath:   "/home/v/cli.sock",
		MCPSockPath:   "/home/v/mcp.sock",
		VectorBackend: vector.BackendSQLiteVec,
		OllamaURL:     "http://localhost:11434",
		EmbedModel:    "nomic-embed-text",
	}
	cp := &configProvider{cfg: cfg}
	m, err := cp.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	checks := map[string]any{
		"veska_home":     "/home/v",
		"sqlite_path":    "/home/v/veska.db",
		"cli_sock":       "/home/v/cli.sock",
		"mcp_sock":       "/home/v/mcp.sock",
		"vector_backend": string(vector.BackendSQLiteVec),
		"ollama_url":     "http://localhost:11434",
		"embed_model":    "nomic-embed-text",
	}
	for k, want := range checks {
		if m[k] != want {
			t.Errorf("%s = %v; want %v", k, m[k], want)
		}
	}
	if _, ok := m["degraded_reasons"].([]string); !ok {
		t.Errorf("degraded_reasons missing or wrong type: %v", m["degraded_reasons"])
	}
}

// newAddRepoTestEnv builds an on-disk SQLite (repo.Add insists on a real DB)
// with the minimal `repos` schema plus a temp git-shaped directory that
// repo.Add will accept.
func newAddRepoTestEnv(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE repos (
		repo_id           TEXT PRIMARY KEY,
		root_path         TEXT NOT NULL UNIQUE,
		added_at          INTEGER NOT NULL,
		active_branch     TEXT,
		last_promoted_sha TEXT,
		module_path       TEXT,
		kind              TEXT NOT NULL DEFAULT 'tracked',
		canonical_url     TEXT,
		last_accessed_at  INTEGER,
		prompted_at       INTEGER
	)`); err != nil {
		t.Fatalf("create repos: %v", err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("make .git/hooks: %v", err)
	}
	return db, root
}

// TestRepoRegistrar_AddRepo_TriggersReparser asserts that a successful repo.Add
// dispatches the cold-scan reparser exactly once in the background, with the
// looked-up RepoRecord, and that AddRepo itself returns promptly without
// blocking on the scan.
func TestRepoRegistrar_AddRepo_TriggersReparser(t *testing.T) {
	db, root := newAddRepoTestEnv(t)

	var (
		wg          sync.WaitGroup
		gotRec      application.RepoRecord
		gotRecMu    sync.Mutex
		invocations atomic.Int32
		release     = make(chan struct{})
	)
	reparser := func(_ context.Context, rec application.RepoRecord) error {
		invocations.Add(1)
		gotRecMu.Lock()
		gotRec = rec
		gotRecMu.Unlock()
		<-release // hold so we can prove AddRepo did not block on the scan
		return nil
	}

	wantBranch := "trunk"
	wantSHA := "deadbeef"
	recordFor := func(_ context.Context, repoID string) (application.RepoRecord, error) {
		return application.RepoRecord{
			RepoID:          repoID,
			RootPath:        root,
			ActiveBranch:    wantBranch,
			LastPromotedSHA: wantSHA,
		}, nil
	}

	rr := &repoRegistrar{
		db:        db,
		reparser:  reparser,
		recordFor: recordFor,
		daemonCtx: context.Background(),
		scanWG:    &wg,
	}

	start := time.Now()
	repoID, _, err := rr.AddRepo(context.Background(), root)
	addElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if repoID == "" {
		t.Fatalf("AddRepo returned empty repoID")
	}
	// 1s is a generous ceiling for "non-blocking" — under -race the harness
	// overhead alone can push a fully-async dispatch into the high-hundreds
	// of milliseconds. The point is to catch a *synchronous wait* on the
	// reparser, which would be many seconds (the blocked reparser holds the
	// goroutine until release is closed below).
	if addElapsed > 1*time.Second {
		t.Errorf("AddRepo blocked for %s (>1s), should be non-blocking", addElapsed)
	}

	// Let the dispatched reparser return.
	close(release)

	// Wait for the goroutine via the WaitGroup, bounded by a 2s timeout.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reparser goroutine did not finish within 2s")
	}

	if got := invocations.Load(); got != 1 {
		t.Errorf("reparser invocations = %d, want 1", got)
	}
	gotRecMu.Lock()
	defer gotRecMu.Unlock()
	if gotRec.RepoID != repoID {
		t.Errorf("reparser RepoID = %q, want %q", gotRec.RepoID, repoID)
	}
	if gotRec.RootPath != root {
		t.Errorf("reparser RootPath = %q, want %q", gotRec.RootPath, root)
	}
	if gotRec.ActiveBranch != wantBranch || gotRec.LastPromotedSHA != wantSHA {
		t.Errorf("reparser record metadata mismatch: %+v", gotRec)
	}
}

// TestRepoRegistrar_AddRepo_ReparserErrorIsNonFatal asserts that a reparser
// failure is logged (not returned) — AddRepo's contract is success on a
// successful registration regardless of the background scan outcome.
func TestRepoRegistrar_AddRepo_ReparserErrorIsNonFatal(t *testing.T) {
	db, root := newAddRepoTestEnv(t)

	var wg sync.WaitGroup
	rr := &repoRegistrar{
		db: db,
		reparser: func(_ context.Context, _ application.RepoRecord) error {
			return errors.New("boom")
		},
		recordFor: func(_ context.Context, repoID string) (application.RepoRecord, error) {
			return application.RepoRecord{RepoID: repoID, RootPath: root}, nil
		},
		daemonCtx: context.Background(),
		scanWG:    &wg,
	}

	if _, _, err := rr.AddRepo(context.Background(), root); err != nil {
		t.Fatalf("AddRepo returned err for reparser failure: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reparser goroutine did not finish within 2s")
	}
}

// TestRepoRegistrar_AddRepo_ContextCanceled asserts that a daemonCtx cancelled
// before dispatch yields a clean exit (no panic, no error from AddRepo). The
// reparser observes ctx.Err and returns context.Canceled, which the registrar
// swallows.
func TestRepoRegistrar_AddRepo_ContextCanceled(t *testing.T) {
	db, root := newAddRepoTestEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	var wg sync.WaitGroup
	rr := &repoRegistrar{
		db: db,
		reparser: func(ctx context.Context, _ application.RepoRecord) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		},
		recordFor: func(ctx context.Context, repoID string) (application.RepoRecord, error) {
			if err := ctx.Err(); err != nil {
				return application.RepoRecord{}, err
			}
			return application.RepoRecord{RepoID: repoID, RootPath: root}, nil
		},
		daemonCtx: ctx,
		scanWG:    &wg,
	}

	if _, _, err := rr.AddRepo(context.Background(), root); err != nil {
		t.Fatalf("AddRepo returned err under cancelled daemonCtx: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reparser goroutine did not finish within 2s after cancel")
	}
}

// TestRepoRegistrar_AddRepo_SeedsLiveWatcher asserts that a successful
// repo.Add invokes watchAdd with the new repoID + rootPath synchronously
// (before AddRepo returns), so subsequent file edits on the freshly-registered
// repo flow through the running fsnotify watcher without a daemon restart
// (solov2-id3).
func TestRepoRegistrar_AddRepo_SeedsLiveWatcher(t *testing.T) {
	db, root := newAddRepoTestEnv(t)

	var (
		gotRepoID string
		gotRoot   string
		called    atomic.Int32
		mu        sync.Mutex
	)
	watchAdd := func(repoID, rootPath string) error {
		called.Add(1)
		mu.Lock()
		gotRepoID, gotRoot = repoID, rootPath
		mu.Unlock()
		return nil
	}

	rr := &repoRegistrar{
		db:        db,
		watchAdd:  watchAdd,
		daemonCtx: context.Background(),
		// nil reparser/recordFor exercises the early-return path so this
		// test focuses on the watcher-seed contract alone.
	}

	repoID, _, err := rr.AddRepo(context.Background(), root)
	if err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("watchAdd invocations = %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotRepoID != repoID {
		t.Errorf("watchAdd repoID = %q, want %q", gotRepoID, repoID)
	}
	if gotRoot != root {
		t.Errorf("watchAdd rootPath = %q, want %q", gotRoot, root)
	}
}

// TestRepoRegistrar_AddRepo_WatcherErrorIsNonFatal asserts that a watchAdd
// failure is logged (not returned) — registration succeeds and the cold scan
// still dispatches even when seeding the live watcher fails. Reasoning:
// daemon restart can recover the watch, but losing registration over a
// transient inotify error would be a worse user-facing failure.
func TestRepoRegistrar_AddRepo_WatcherErrorIsNonFatal(t *testing.T) {
	db, root := newAddRepoTestEnv(t)

	var wg sync.WaitGroup
	reparserCalled := atomic.Int32{}
	rr := &repoRegistrar{
		db: db,
		watchAdd: func(string, string) error {
			return errors.New("inotify watch limit reached")
		},
		reparser: func(_ context.Context, _ application.RepoRecord) error {
			reparserCalled.Add(1)
			return nil
		},
		recordFor: func(_ context.Context, repoID string) (application.RepoRecord, error) {
			return application.RepoRecord{RepoID: repoID, RootPath: root}, nil
		},
		daemonCtx: context.Background(),
		scanWG:    &wg,
	}

	if _, _, err := rr.AddRepo(context.Background(), root); err != nil {
		t.Fatalf("AddRepo returned err on watchAdd failure: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reparser goroutine did not finish within 2s")
	}
	if got := reparserCalled.Load(); got != 1 {
		t.Errorf("reparser invocations after watchAdd failure = %d, want 1", got)
	}
}

// TestRepoRegistrar_AddRepo_ReDispatchesScanForDirectWriteRow covers solov2-0bc1:
// a repo row inserted via the CLI's direct-write fallback (no daemon
// running) has no last_promoted_sha and was never indexed. Re-running
// `veska repo add` after the daemon starts must dispatch a cold-scan for
// that row, not silently report "already registered" without scanning.
func TestRepoRegistrar_AddRepo_ReDispatchesScanForDirectWriteRow(t *testing.T) {
	db, root := newAddRepoTestEnv(t)

	// Pre-insert the repo row (simulates the direct-write fallback path).
	// The first AddRepo call below sees existed=true.
	if _, _, err := repo.Add(context.Background(), db, root); err != nil {
		t.Fatalf("seed direct-write row: %v", err)
	}

	var wg sync.WaitGroup
	reparserCalled := atomic.Int32{}
	rr := &repoRegistrar{
		db: db,
		reparser: func(_ context.Context, _ application.RepoRecord) error {
			reparserCalled.Add(1)
			return nil
		},
		// recordFor returns a record with EMPTY LastPromotedSHA — the
		// signature of a direct-write row that was never scanned.
		recordFor: func(_ context.Context, repoID string) (application.RepoRecord, error) {
			return application.RepoRecord{RepoID: repoID, RootPath: root, LastPromotedSHA: ""}, nil
		},
		daemonCtx: context.Background(),
		scanWG:    &wg,
	}

	_, existed, err := rr.AddRepo(context.Background(), root)
	if err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if !existed {
		t.Fatalf("want existed=true on second add, got false")
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reparser goroutine did not finish within 2s")
	}
	if got := reparserCalled.Load(); got != 1 {
		t.Errorf("direct-write row must trigger a fresh cold-scan; reparser invocations = %d, want 1", got)
	}
}

// TestRepoRegistrar_AddRepo_SkipsScanForPromotedRow guards the
// other side of the solov2-0bc1 condition: a row with a non-empty
// last_promoted_sha was already indexed, and re-adding must NOT
// re-dispatch the cold scan (preserves the prior solov2-khjd skip).
func TestRepoRegistrar_AddRepo_SkipsScanForPromotedRow(t *testing.T) {
	db, root := newAddRepoTestEnv(t)
	if _, _, err := repo.Add(context.Background(), db, root); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	var wg sync.WaitGroup
	reparserCalled := atomic.Int32{}
	rr := &repoRegistrar{
		db: db,
		reparser: func(_ context.Context, _ application.RepoRecord) error {
			reparserCalled.Add(1)
			return nil
		},
		recordFor: func(_ context.Context, repoID string) (application.RepoRecord, error) {
			return application.RepoRecord{RepoID: repoID, RootPath: root, LastPromotedSHA: "abc123"}, nil
		},
		daemonCtx: context.Background(),
		scanWG:    &wg,
	}

	if _, _, err := rr.AddRepo(context.Background(), root); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	// Give any erroneous goroutine a beat to start.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
	if got := reparserCalled.Load(); got != 0 {
		t.Errorf("promoted row must NOT trigger a fresh cold-scan; reparser invocations = %d, want 0", got)
	}
}
