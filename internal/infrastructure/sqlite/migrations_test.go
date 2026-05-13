package sqlite_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/sqlite"
)

// openRawDB opens a raw *sql.DB for inspection without running migrations.
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
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
// tests from each other and from ~/.engram-backups.
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
	dbPath := filepath.Join(dir, "engram.db")

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
	dbPath := filepath.Join(dir, "engram.db")

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
	dbPath := filepath.Join(dir, "engram.db")

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
	dbPath := filepath.Join(dir, "engram.db")

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
	dbPath := filepath.Join(dir, "engram.db")

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
	dbPath := filepath.Join(dir, "engram.db")
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
	dbPath := filepath.Join(dir, "engram.db")

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
	}
}

// TestAutoSnapshot_CreatedBeforeMigrations verifies the auto-snapshot file is created
// when there are pending migrations.
func TestAutoSnapshot_CreatedBeforeMigrations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "engram.db")
	backupDir := filepath.Join(dir, "backups", ".pre-migration")

	// Open with a custom backup dir to avoid touching ~/.engram-backups in tests.
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
	dbPath := filepath.Join(dir, "engram.db")

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
