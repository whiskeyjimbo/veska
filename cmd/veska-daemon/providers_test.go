package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
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
			module_path       TEXT
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
	if m["status"] != "ok" {
		t.Errorf("status = %v; want ok", m["status"])
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
	if _, ok := m["degraded_reasons"].([]string); !ok {
		t.Errorf("degraded_reasons missing or wrong type: %v", m["degraded_reasons"])
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
		module_path       TEXT
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
	repoID, err := rr.AddRepo(context.Background(), root)
	addElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	if repoID == "" {
		t.Fatalf("AddRepo returned empty repoID")
	}
	if addElapsed > 200*time.Millisecond {
		t.Errorf("AddRepo blocked for %s (>200ms), should be non-blocking", addElapsed)
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

	if _, err := rr.AddRepo(context.Background(), root); err != nil {
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

	if _, err := rr.AddRepo(context.Background(), root); err != nil {
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
