package main

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

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
