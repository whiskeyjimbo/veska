package sqlite_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// openRawDB opens a raw *sql.DB for inspection without running migrations.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("openRawDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// tableExists returns true if the named table exists in db.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var cnt int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&cnt)
	if err != nil {
		t.Fatalf("tableExists %s: %v", name, err)
	}
	return cnt > 0
}

// indexExists returns true if the named index exists in db.
func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var cnt int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&cnt)
	if err != nil {
		t.Fatalf("indexExists %s: %v", name, err)
	}
	return cnt > 0
}

// openTest opens a DB using OpenWithOptions with a temp backup dir, isolating
// tests from each other and from ~/.veska-backups.
func openTest(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMigration0001_CreatesAllTables verifies migration 0001 creates all required tables.
func TestMigration0001_CreatesAllTables(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	db := openTest(t, dbPath)
	_ = db

	raw := openRawDB(t, dbPath)

	expectedTables := []string{
		"schema_migrations",
		"database_meta",
		"daemon_state",
		"repos",
		"nodes",
		"edges",
		"cross_repo_edge_stubs",
	}
	for _, tbl := range expectedTables {
		if !tableExists(t, raw, tbl) {
			t.Errorf("table %q not found after migration 0001", tbl)
		}
	}
}

// TestMigration0001_CreatesIndexes verifies indexes from migration 0001.
func TestMigration0001_CreatesIndexes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)

	raw := openRawDB(t, dbPath)

	expectedIndexes := []string{
		"idx_nodes_repo_branch",
		"idx_nodes_symbol",
		"idx_nodes_content_hash",
		"idx_edges_src",
		"idx_edges_dst",
		"idx_edges_repo_branch",
		"idx_stubs_src",
		"idx_stubs_resolver",
		"idx_stubs_repo_branch",
	}
	for _, idx := range expectedIndexes {
		if !indexExists(t, raw, idx) {
			t.Errorf("index %q not found after migration 0001", idx)
		}
	}
}

// TestMigration0001_RecordsSchemaVersion verifies schema_migrations has version 1 after open.
func TestMigration0001_RecordsSchemaVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)

	raw := openRawDB(t, dbPath)

	var version int
	var appliedAt int64
	var binarySHA, migrationSHA, appliedBy string
	err := raw.QueryRow(`SELECT version, applied_at, binary_sha, migration_sha, applied_by FROM schema_migrations WHERE version = 1`).
		Scan(&version, &appliedAt, &binarySHA, &migrationSHA, &appliedBy)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if version != 1 {
		t.Errorf("expected version 1, got %d", version)
	}
	if appliedAt <= 0 {
		t.Errorf("applied_at should be positive, got %d", appliedAt)
	}
	if migrationSHA == "" {
		t.Error("migration_sha should not be empty")
	}
	if appliedBy == "" {
		t.Error("applied_by should not be empty")
	}
}

// TestOpen_WALModeEnabled verifies WAL journal mode is set.
func TestOpen_WALModeEnabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)

	raw := openRawDB(t, dbPath)

	var mode string
	if err := raw.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("expected journal_mode=wal, got %q", mode)
	}
}

// TestOpen_WALAutocheckpoint verifies wal_autocheckpoint=1000.
func TestOpen_WALAutocheckpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)

	raw := openRawDB(t, dbPath)

	var checkpoint int
	if err := raw.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&checkpoint); err != nil {
		t.Fatalf("PRAGMA wal_autocheckpoint: %v", err)
	}
	if checkpoint != 1000 {
		t.Errorf("expected wal_autocheckpoint=1000, got %d", checkpoint)
	}
}

// TestOpen_Idempotent verifies opening an already-migrated DB does not re-run migrations.
func TestOpen_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db1, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("first OpenWithOptions: %v", err)
	}
	db1.Close()

	db2, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("second OpenWithOptions: %v", err)
	}
	defer db2.Close()

	raw := openRawDB(t, dbPath)
	var cnt int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&cnt); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	// Count should equal the number of migrations in the registry — each applied exactly once.
	wantMigrations := sqlite.MigrationCount()
	if cnt != wantMigrations {
		t.Errorf("expected exactly %d migration row(s), got %d", wantMigrations, cnt)
	}
}

// TestMigrationSHA_TamperDetected verifies exit 78 behaviour when migration_sha is modified.
// We exercise CheckMigrationIntegrity directly after tampering.
func TestMigrationSHA_TamperDetected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	// Open normally to apply migration 0001.
	db := openTest(t, dbPath)
	db.Close()

	// Tamper: update the migration_sha to a bogus value.
	raw := openRawDB(t, dbPath)
	if _, err := raw.Exec(`UPDATE schema_migrations SET migration_sha = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper migration_sha: %v", err)
	}
	raw.Close()

	err := sqlite.CheckMigrationIntegrity(dbPath)
	if err == nil {
		t.Fatal("expected integrity error for tampered migration_sha, got nil")
		return
	}
}

// TestAutoSnapshot_CreatedBeforeMigrations verifies the auto-snapshot file is created
// when there are pending migrations.
func TestAutoSnapshot_CreatedBeforeMigrations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(dir, "backups", ".pre-migration")

	// Open with a custom backup dir to avoid touching ~/.veska-backups in tests.
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{
		BackupDir: backupDir,
	})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one snapshot file in backup dir")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Snapshot name should be: 0-1-<timestamp>.db
		if filepath.Ext(name) != ".db" {
			t.Errorf("unexpected file in backup dir: %s", name)
		}
	}
}

// TestWALReplay_KillMidMigration verifies that if the process is killed during migration
// (simulated by closing DB before commit), WAL replay leaves the DB in its pre-migration state.
func TestWALReplay_KillMidMigration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	// Open a raw db, set WAL, create schema_migrations table, but do NOT commit 0001.
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	// Set WAL mode.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("set WAL: %v", err)
	}

	// Create schema_migrations manually.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version       INTEGER PRIMARY KEY,
		applied_at    INTEGER NOT NULL,
		binary_sha    TEXT NOT NULL,
		migration_sha TEXT NOT NULL,
		applied_by    TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	// Begin a transaction, insert into schema_migrations, then "kill" by closing without commit.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}

	if _, err := tx.Exec(`INSERT INTO schema_migrations VALUES (1, ?, '', '', 'test')`, time.Now().Unix()); err != nil {
		tx.Rollback() //nolint:errcheck
		t.Fatalf("insert in tx: %v", err)
	}

	// Simulate kill: close without committing — triggers WAL rollback.
	tx.Rollback() //nolint:errcheck
	db.Close()

	// Reopen: schema_migrations should have 0 rows (WAL rolled back).
	db2, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	var cnt int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&cnt); err != nil {
		t.Fatalf("count after WAL rollback: %v", err)
	}
	if cnt != 0 {
		t.Errorf("expected 0 rows after WAL rollback (kill-mid-migration), got %d", cnt)
	}
}

// TestMigrationSHA_Computation verifies the canonical SHA-256 computation.
func TestMigrationSHA_Computation(t *testing.T) {
	t.Parallel()

	// Known input/output: SHA-256 of "hello\n" (UTF-8, no BOM).
	// echo -n "hello\n" | sha256sum  => not what we want
	// We want SHA-256 of the 6-byte string "hello\n":
	// sha256("hello\n") = 5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03
	got := sqlite.ComputeMigrationSHA("hello\n")
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got != want {
		t.Errorf("ComputeMigrationSHA(hello\\n) = %q, want %q", got, want)
	}
}

// TestMigrationSHA_StripsBOM verifies UTF-8 BOM is stripped before SHA computation.
func TestMigrationSHA_StripsBOM(t *testing.T) {
	t.Parallel()

	// BOM = 0xEF 0xBB 0xBF
	withBOM := "\xef\xbb\xbfhello\n"
	withoutBOM := "hello\n"
	if sqlite.ComputeMigrationSHA(withBOM) != sqlite.ComputeMigrationSHA(withoutBOM) {
		t.Error("BOM stripping failed: SHA differs for BOM vs no-BOM input")
	}
}

// TestMigrationSHA_NormalisesLineEndings verifies CRLF → LF normalisation.
func TestMigrationSHA_NormalisesLineEndings(t *testing.T) {
	t.Parallel()

	crlf := "hello\r\nworld\r\n"
	lf := "hello\nworld\n"
	if sqlite.ComputeMigrationSHA(crlf) != sqlite.ComputeMigrationSHA(lf) {
		t.Error("line-ending normalisation failed: SHA differs for CRLF vs LF input")
	}
}

// ---------------------------------------------------------------------------
// Migration 0003: tasks, findings, suppressions
// ---------------------------------------------------------------------------

// TestMigration0003_TablesAndIndexesExist verifies all three tables and their
// indexes are created by migration 0003.
func TestMigration0003_TablesAndIndexesExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)

	raw := openRawDB(t, dbPath)

	expectedTables := []string{"tasks", "findings", "suppressions"}
	for _, tbl := range expectedTables {
		if !tableExists(t, raw, tbl) {
			t.Errorf("table %q not found after migration 0003", tbl)
		}
	}

	expectedIndexes := []string{
		"idx_tasks_active_one_per_repo",
		"idx_findings_state",
		"idx_findings_anchor",
		"idx_findings_repo_branch",
		"idx_suppressions_target",
	}
	for _, idx := range expectedIndexes {
		if !indexExists(t, raw, idx) {
			t.Errorf("index %q not found after migration 0003", idx)
		}
	}
}

// TestMigration0003_TasksActivePartialIndex verifies the partial unique index
// idx_tasks_active_one_per_repo: only one active task per repo is allowed.
func TestMigration0003_TasksActivePartialIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()

	// Insert a repo first (tasks has FK to repos).
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo-1", "/tmp/repo1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Insert first active task — must succeed.
	if _, err := db.Exec(`INSERT INTO tasks (task_id, repo_id, title, active, created_at) VALUES (?, ?, ?, ?, ?)`,
		"task-1", "repo-1", "First Task", 1, now); err != nil {
		t.Fatalf("insert first active task: %v", err)
	}

	// Insert second active task for same repo — must fail (partial unique index).
	_, err = db.Exec(`INSERT INTO tasks (task_id, repo_id, title, active, created_at) VALUES (?, ?, ?, ?, ?)`,
		"task-2", "repo-1", "Second Task", 1, now)
	if err == nil {
		t.Fatal("expected unique constraint violation for second active task on same repo, got nil")
		return
	}

	// Insert inactive task for same repo — must succeed.
	if _, err := db.Exec(`INSERT INTO tasks (task_id, repo_id, title, active, created_at) VALUES (?, ?, ?, ?, ?)`,
		"task-3", "repo-1", "Inactive Task", 0, now); err != nil {
		t.Fatalf("insert inactive task should succeed: %v", err)
	}
}

// TestMigration0003_FindingsBranchPK verifies that the same finding_id can
// coexist on two different branches (open on A, closed on B).
func TestMigration0003_FindingsBranchPK(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()

	// Insert a repo for FK.
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo-2", "/tmp/repo2", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	const findingID = "finding-abc"

	// Same finding_id open on branch A.
	if _, err := db.Exec(`INSERT INTO findings
		(finding_id, branch, repo_id, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		findingID, "branch-a", "repo-2", "warn", "linter", "rule-x", "msg", "open", now, "agent-1", "agent"); err != nil {
		t.Fatalf("insert finding on branch-a: %v", err)
	}

	// Same finding_id closed on branch B — must succeed (different PK).
	if _, err := db.Exec(`INSERT INTO findings
		(finding_id, branch, repo_id, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		findingID, "branch-b", "repo-2", "warn", "linter", "rule-x", "msg", "closed", now, "agent-1", "agent"); err != nil {
		t.Fatalf("insert same finding_id on branch-b should succeed: %v", err)
	}

	// Duplicate (finding_id, branch) must fail.
	_, err = db.Exec(`INSERT INTO findings
		(finding_id, branch, repo_id, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		findingID, "branch-a", "repo-2", "warn", "linter", "rule-x", "dup", "open", now, "agent-1", "agent")
	if err == nil {
		t.Fatal("expected PK violation for duplicate (finding_id, branch), got nil")
		return
	}

	// Verify both rows exist with their respective states.
	var stateA, stateB string
	if err := db.QueryRow(`SELECT state FROM findings WHERE finding_id=? AND branch=?`, findingID, "branch-a").Scan(&stateA); err != nil {
		t.Fatalf("query branch-a finding: %v", err)
	}
	if err := db.QueryRow(`SELECT state FROM findings WHERE finding_id=? AND branch=?`, findingID, "branch-b").Scan(&stateB); err != nil {
		t.Fatalf("query branch-b finding: %v", err)
	}
	if stateA != "open" {
		t.Errorf("branch-a finding state: want open, got %s", stateA)
	}
	if stateB != "closed" {
		t.Errorf("branch-b finding state: want closed, got %s", stateB)
	}
}

// ---------------------------------------------------------------------------
// Migration 0004: node_embeddings, node_embedding_refs, node_fts
// ---------------------------------------------------------------------------

// TestMigration0004_TablesExist verifies migration 0004 creates all three objects.
func TestMigration0004_TablesExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)

	raw := openRawDB(t, dbPath)

	for _, tbl := range []string{"node_embeddings", "node_embedding_refs"} {
		if !tableExists(t, raw, tbl) {
			t.Errorf("table %q not found after migration 0004", tbl)
		}
	}

	// node_fts was created in migration 0004 and replaced by
	// node_fts_words + node_fts_trigrams in migration 0007. Final-state
	// assertion: the original is gone, the two replacements exist.
	var cnt int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='node_fts'`).Scan(&cnt); err != nil {
		t.Fatalf("check node_fts absence: %v", err)
	}
	if cnt != 0 {
		t.Error("legacy node_fts should be dropped by migration 0007")
	}
	for _, tbl := range []string{"node_fts_words", "node_fts_trigrams"} {
		if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, tbl).Scan(&cnt); err != nil {
			t.Fatalf("check %s: %v", tbl, err)
		}
		if cnt == 0 {
			t.Errorf("virtual table %s not found after migration 0007", tbl)
		}
	}

	if !indexExists(t, raw, "idx_node_embedding_refs_state") {
		t.Error("index idx_node_embedding_refs_state not found after migration 0004")
	}
}

// TestMigration0004_ContentAddressedDedup verifies inserting the same content_hash
// twice into node_embeddings fails (PRIMARY KEY constraint).
func TestMigration0004_ContentAddressedDedup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()
	embedding := []byte{0x01, 0x02, 0x03, 0x04}

	if _, err := db.Exec(`INSERT INTO node_embeddings (content_hash, model, dim, embedding, created_at) VALUES (?, ?, ?, ?, ?)`,
		"hash-abc", "nomic-embed-text", 768, embedding, now); err != nil {
		t.Fatalf("first insert into node_embeddings: %v", err)
	}

	_, err = db.Exec(`INSERT INTO node_embeddings (content_hash, model, dim, embedding, created_at) VALUES (?, ?, ?, ?, ?)`,
		"hash-abc", "nomic-embed-text", 768, embedding, now)
	if err == nil {
		t.Fatal("expected PK violation for duplicate content_hash, got nil")
		return
	}
}

// TestMigration0004_TwoNodesShareEmbedding verifies two nodes can reference the same
// content_hash in node_embedding_refs (the deduplication point).
func TestMigration0004_TwoNodesShareEmbedding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()

	// Insert repo and two nodes (migration 0001 tables).
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo-emb", "/tmp/repo-emb", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	for _, nodeID := range []string{"node-A", "node-B"} {
		if _, err := db.Exec(`INSERT INTO nodes (node_id, branch, repo_id, language, kind, symbol_path, file_path, content_hash, last_promoted_at, actor_id, actor_kind) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, "main", "repo-emb", "go", "func", "pkg.Sym", "file.go", "chash-xyz", now, "agent-1", "agent"); err != nil {
			t.Fatalf("insert node %s: %v", nodeID, err)
		}
	}

	// Insert a shared embedding.
	if _, err := db.Exec(`INSERT INTO node_embeddings (content_hash, model, dim, embedding, created_at) VALUES (?, ?, ?, ?, ?)`,
		"shared-hash", "nomic-embed-text", 768, []byte{0xDE, 0xAD}, now); err != nil {
		t.Fatalf("insert shared embedding: %v", err)
	}

	// Both nodes reference the same embedding.
	for _, nodeID := range []string{"node-A", "node-B"} {
		if _, err := db.Exec(`INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at, embedded_at) VALUES (?, ?, ?, ?, ?)`,
			nodeID, "shared-hash", "ready", now, now); err != nil {
			t.Fatalf("insert node_embedding_refs for %s: %v", nodeID, err)
		}
	}

	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE content_hash=?`, "shared-hash").Scan(&cnt); err != nil {
		t.Fatalf("count refs: %v", err)
	}
	if cnt != 2 {
		t.Errorf("expected 2 refs sharing the same embedding, got %d", cnt)
	}
}

// TestMigration0004_StateTransitions verifies node_embedding_refs pending->ready
// transition and FK enforcement on content_hash.
func TestMigration0004_StateTransitions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()

	// Insert repo and node.
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo-st", "/tmp/repo-st", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (node_id, branch, repo_id, language, kind, symbol_path, file_path, content_hash, last_promoted_at, actor_id, actor_kind) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"node-st", "main", "repo-st", "go", "func", "pkg.F", "f.go", "chash-st", now, "agent-1", "agent"); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	// Insert pending (content_hash NULL).
	if _, err := db.Exec(`INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at) VALUES (?, NULL, ?, ?)`,
		"node-st", "pending", now); err != nil {
		t.Fatalf("insert pending ref: %v", err)
	}

	var state string
	var contentHash sql.NullString
	if err := db.QueryRow(`SELECT state, content_hash FROM node_embedding_refs WHERE node_id=?`, "node-st").Scan(&state, &contentHash); err != nil {
		t.Fatalf("query pending ref: %v", err)
	}
	if state != "pending" {
		t.Errorf("expected state=pending, got %s", state)
	}
	if contentHash.Valid {
		t.Errorf("expected content_hash NULL in pending state, got %s", contentHash.String)
	}

	// Insert the embedding.
	if _, err := db.Exec(`INSERT INTO node_embeddings (content_hash, model, dim, embedding, created_at) VALUES (?, ?, ?, ?, ?)`,
		"hash-st", "nomic-embed-text", 768, []byte{0xFF}, now); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}

	// Transition to ready with valid content_hash.
	if _, err := db.Exec(`UPDATE node_embedding_refs SET state=?, content_hash=?, embedded_at=? WHERE node_id=?`,
		"ready", "hash-st", now, "node-st"); err != nil {
		t.Fatalf("transition to ready: %v", err)
	}

	if err := db.QueryRow(`SELECT state, content_hash FROM node_embedding_refs WHERE node_id=?`, "node-st").Scan(&state, &contentHash); err != nil {
		t.Fatalf("query ready ref: %v", err)
	}
	if state != "ready" {
		t.Errorf("expected state=ready, got %s", state)
	}
	if !contentHash.Valid || contentHash.String != "hash-st" {
		t.Errorf("expected content_hash=hash-st, got %v", contentHash)
	}

	// FK violation: update to a non-existent content_hash should fail.
	_, err = db.Exec(`UPDATE node_embedding_refs SET content_hash=? WHERE node_id=?`, "nonexistent-hash", "node-st")
	if err == nil {
		t.Fatal("expected FK violation for nonexistent content_hash, got nil")
		return
	}
}

// TestMigration0007_FTSWordsAndTrigramsQueryable verifies the m3.03.2 FTS
// pair can be inserted into and queried for both prefix matches (words) and
// substring matches (trigrams).
func TestMigration0007_FTSWordsAndTrigramsQueryable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	// Pre-tokenised words: kind + symbol_path + name with camelCase split.
	if _, err := db.Exec(
		`INSERT INTO node_fts_words (node_id, branch, repo_id, words) VALUES (?, ?, ?, ?)`,
		"n1", "main", "repo-fts",
		"function pkg api closeFinding close Finding",
	); err != nil {
		t.Fatalf("insert node_fts_words: %v", err)
	}

	// Raw form for trigrams: substring matchable.
	if _, err := db.Exec(
		`INSERT INTO node_fts_trigrams (node_id, branch, repo_id, raw) VALUES (?, ?, ?, ?)`,
		"n1", "main", "repo-fts", "function pkg/api closeFinding",
	); err != nil {
		t.Fatalf("insert node_fts_trigrams: %v", err)
	}

	// Words: prefix match on "close" should hit (camelCase split).
	var got string
	if err := db.QueryRow(
		`SELECT node_id FROM node_fts_words WHERE words MATCH ? LIMIT 1`,
		"close*",
	).Scan(&got); err != nil {
		t.Fatalf("node_fts_words MATCH close*: %v", err)
	}
	if got != "n1" {
		t.Errorf("words match: want n1, got %q", got)
	}

	// Trigrams: substring "ind" appears in "closeFinding".
	if err := db.QueryRow(
		`SELECT node_id FROM node_fts_trigrams WHERE raw MATCH ? LIMIT 1`,
		"ind",
	).Scan(&got); err != nil {
		t.Fatalf("node_fts_trigrams MATCH ind: %v", err)
	}
	if got != "n1" {
		t.Errorf("trigrams match: want n1, got %q", got)
	}
}

// TestMigration0003_SuppressionsAgnosticVsSpecific verifies branch-agnostic
// (branch IS NULL) and branch-specific suppressions can coexist.
func TestMigration0003_SuppressionsAgnosticVsSpecific(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()

	// Branch-agnostic suppression (branch IS NULL).
	if _, err := db.Exec(`INSERT INTO suppressions
		(suppression_id, scope, target, branch, rule, reason, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sup-1", "rule", "rule-x", nil, "rule-x", "known false positive", now, "agent-1", "agent"); err != nil {
		t.Fatalf("insert branch-agnostic suppression: %v", err)
	}

	// Branch-specific suppression (branch = 'feat/x').
	if _, err := db.Exec(`INSERT INTO suppressions
		(suppression_id, scope, target, branch, rule, reason, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sup-2", "rule", "rule-x", "feat/x", "rule-x", "branch-specific override", now, "agent-1", "agent"); err != nil {
		t.Fatalf("insert branch-specific suppression: %v", err)
	}

	// Verify branch-agnostic row has NULL branch.
	var branchVal sql.NullString
	if err := db.QueryRow(`SELECT branch FROM suppressions WHERE suppression_id=?`, "sup-1").Scan(&branchVal); err != nil {
		t.Fatalf("query sup-1: %v", err)
	}
	if branchVal.Valid {
		t.Errorf("sup-1 branch should be NULL, got %q", branchVal.String)
	}

	// Verify branch-specific row has correct branch.
	if err := db.QueryRow(`SELECT branch FROM suppressions WHERE suppression_id=?`, "sup-2").Scan(&branchVal); err != nil {
		t.Fatalf("query sup-2: %v", err)
	}
	if !branchVal.Valid || branchVal.String != "feat/x" {
		t.Errorf("sup-2 branch: want feat/x, got %v", branchVal)
	}

	// Count suppressions matching target regardless of branch (agnostic lookup).
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions WHERE target=?`, "rule-x").Scan(&cnt); err != nil {
		t.Fatalf("count suppressions: %v", err)
	}
	if cnt != 2 {
		t.Errorf("expected 2 suppressions for target rule-x, got %d", cnt)
	}

	// Count branch-specific suppressions for feat/x only.
	if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions WHERE target=? AND branch=?`, "rule-x", "feat/x").Scan(&cnt); err != nil {
		t.Fatalf("count branch-specific suppressions: %v", err)
	}
	if cnt != 1 {
		t.Errorf("expected 1 branch-specific suppression for feat/x, got %d", cnt)
	}
}

// ── Migration 0005: nodes.signature + nodes.prev_signature ─────────────────

// columnExists returns true if the named column is present on the table.
func columnExists(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		if n == col {
			return true
		}
	}
	return false
}

// TestMigration0005_AddsSignatureColumns verifies migration 0005 adds the two
// new columns to nodes with NULL as the default value.
func TestMigration0005_AddsSignatureColumns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)
	raw := openRawDB(t, dbPath)

	if !columnExists(t, raw, "nodes", "signature") {
		t.Error("nodes.signature column not found after migration 0005")
	}
	if !columnExists(t, raw, "nodes", "prev_signature") {
		t.Error("nodes.prev_signature column not found after migration 0005")
	}
}

// ── Migration 0006: node_embedding_refs.attempts ───────────────────────────

// TestMigration0006_AddsAttemptsColumn verifies migration 0006 adds the
// attempts column to node_embedding_refs.
func TestMigration0006_AddsAttemptsColumn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)
	raw := openRawDB(t, dbPath)

	if !columnExists(t, raw, "node_embedding_refs", "attempts") {
		t.Error("node_embedding_refs.attempts column not found after migration 0006")
	}
}

// TestMigration0006_AttemptsDefaultsToZero verifies new rows omitting the
// attempts column default to 0.
func TestMigration0006_AttemptsDefaultsToZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	db := openTest(t, dbPath)

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"n1", "main", "r1", "go", "function", "F", "a.go", "h", now, "test", "system"); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?,?,?)`,
		"n1", "pending", now); err != nil {
		t.Fatalf("insert ref: %v", err)
	}

	var attempts int
	if err := db.QueryRow(`SELECT attempts FROM node_embedding_refs WHERE node_id=?`, "n1").Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts default: want 0, got %d", attempts)
	}
}

// TestMigration0005_DefaultsAreNull verifies that legacy INSERT statements
// (omitting the new columns) yield NULL for both new columns.
func TestMigration0005_DefaultsAreNull(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	db := openTest(t, dbPath)

	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"n1", "main", "r1", "go", "function", "F", "a.go",
		"h", time.Now().UnixMilli(), "service:veska", "system"); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	var sig, prev sql.NullString
	if err := db.QueryRow(`SELECT signature, prev_signature FROM nodes WHERE node_id=?`, "n1").Scan(&sig, &prev); err != nil {
		t.Fatalf("query node: %v", err)
	}
	if sig.Valid {
		t.Errorf("signature: want NULL, got %q", sig.String)
	}
	if prev.Valid {
		t.Errorf("prev_signature: want NULL, got %q", prev.String)
	}
}

// ── Migration 0008: findings.anchor_content_hash ───────────────────────────

// TestMigration0008_AddsAnchorContentHashColumn verifies migration 0008 adds
// the nullable anchor_content_hash column to findings.
func TestMigration0008_AddsAnchorContentHashColumn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)
	raw := openRawDB(t, dbPath)

	if !columnExists(t, raw, "findings", "anchor_content_hash") {
		t.Error("findings.anchor_content_hash column not found after migration 0008")
	}
}

// TestMigration0008_PreservesExistingRowsAsNull verifies that an INSERT
// omitting anchor_content_hash leaves the column NULL — the contract the
// revalidation sweep relies on for "no hash recorded, fall back to anchor
// existence check".
func TestMigration0008_PreservesExistingRowsAsNull(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	db := openTest(t, dbPath)

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"r1", "/tmp/r1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO findings (
		finding_id, branch, repo_id, file_path,
		severity, source_layer, rule, message, state,
		created_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"fid", "main", "r1", "foo.go",
		"low", "structural", "parse-failure", "msg", "open",
		time.Now().UnixMilli(), "service:veska", "system",
	); err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	var got sql.NullString
	if err := db.QueryRow(
		`SELECT anchor_content_hash FROM findings WHERE finding_id = 'fid'`,
	).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Valid {
		t.Errorf("anchor_content_hash should default to NULL, got %q", got.String)
	}
}

// ── Migration 0009: nodes.snippet ──────────────────────────────────────────

// TestMigration0009_AddsSnippetColumn verifies migration 0009 adds the
// nullable snippet column to nodes.
func TestMigration0009_AddsSnippetColumn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	_ = openTest(t, dbPath)
	raw := openRawDB(t, dbPath)

	if !columnExists(t, raw, "nodes", "snippet") {
		t.Error("nodes.snippet column not found after migration 0009")
	}
}

// TestMigration0009_DefaultsToNull verifies that an INSERT omitting snippet
// leaves the column NULL — the contract the +snippet embed-text projection
// relies on (domain.EmbedText skips empty parts, degrading to baseline).
func TestMigration0009_DefaultsToNull(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")

	db := openTest(t, dbPath)

	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"n1", "main", "r1", "go", "function", "F", "a.go",
		"h", time.Now().UnixMilli(), "service:veska", "system"); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	var got sql.NullString
	if err := db.QueryRow(`SELECT snippet FROM nodes WHERE node_id = 'n1'`).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Valid {
		t.Errorf("snippet should default to NULL, got %q", got.String)
	}
}

// ── Migration 0013: repos cache-tier columns (solov2-kxo5.2) ───────────────

func TestMigration0013_AddsCacheTierColumns(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "veska.db")
	_ = openTest(t, dbPath)
	raw := openRawDB(t, dbPath)

	for _, col := range []string{"kind", "canonical_url", "last_accessed_at", "prompted_at"} {
		if !columnExists(t, raw, "repos", col) {
			t.Errorf("repos.%s missing after migration 0013", col)
		}
	}
	if !indexExists(t, raw, "idx_repos_canonical_url") {
		t.Error("idx_repos_canonical_url missing after migration 0013")
	}
}

func TestMigration0013_DefaultsForExistingRow(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "veska.db")
	db := openTest(t, dbPath)

	// Insert a row simulating "already-registered before the migration",
	// using only the pre-0013 columns. The DEFAULT on kind plus the
	// nullability of the other three columns should let this succeed.
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"abc123", "/tmp/foo", int64(1000),
	); err != nil {
		t.Fatalf("insert existing-style row: %v", err)
	}

	raw := openRawDB(t, dbPath)
	var kind string
	var canonical, lastAccessed, promptedAt sql.NullString
	err := raw.QueryRow(
		`SELECT kind, canonical_url, last_accessed_at, prompted_at FROM repos WHERE repo_id = ?`,
		"abc123",
	).Scan(&kind, &canonical, &lastAccessed, &promptedAt)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}
	if kind != "tracked" {
		t.Errorf("kind default: want 'tracked', got %q", kind)
	}
	if canonical.Valid {
		t.Errorf("canonical_url should default to NULL, got %q", canonical.String)
	}
	if lastAccessed.Valid {
		t.Errorf("last_accessed_at should default to NULL, got %q", lastAccessed.String)
	}
	if promptedAt.Valid {
		t.Errorf("prompted_at should default to NULL, got %q", promptedAt.String)
	}
}

func TestMigration0013_CanonicalURLUniquePartialIndex(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "veska.db")
	db := openTest(t, dbPath)

	// Two rows with NULL canonical_url are allowed (partial index excludes NULL).
	for i, id := range []string{"id-null-1", "id-null-2"} {
		if _, err := db.Exec(
			`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
			id, fmt.Sprintf("/tmp/null-%d", i), int64(1000+i),
		); err != nil {
			t.Fatalf("insert NULL canonical_url row %d: %v", i, err)
		}
	}

	// First non-NULL value succeeds.
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, canonical_url) VALUES (?, ?, ?, ?)`,
		"id-url-1", "/tmp/u1", int64(2000), "https://github.com/foo/bar",
	); err != nil {
		t.Fatalf("first non-NULL canonical_url: %v", err)
	}

	// Duplicate non-NULL value must fail.
	_, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, canonical_url) VALUES (?, ?, ?, ?)`,
		"id-url-2", "/tmp/u2", int64(2001), "https://github.com/foo/bar",
	)
	if err == nil {
		t.Fatal("duplicate canonical_url should violate unique partial index, got nil")
	}
}
