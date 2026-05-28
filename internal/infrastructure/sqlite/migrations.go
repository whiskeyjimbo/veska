package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration describes a single versioned schema change.
type migration struct {
	version int
	name    string
	sql     string
}

// migrations is the ordered registry of all known migrations.
// New migrations must be appended; never reorder or remove.
var migrations = func() []migration {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		panic("sqlite: cannot read embedded migrations: " + err.Error())
	}
	ms := make([]migration, 0, len(entries))
	for i, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			panic("sqlite: cannot read migration " + e.Name() + ": " + err.Error())
		}
		ms = append(ms, migration{
			version: i + 1,
			name:    e.Name(),
			sql:     string(data),
		})
	}
	return ms
}()

// minSchemaVersion is the oldest schema version this binary accepts.
const minSchemaVersion = 1

// ComputeMigrationSHA returns the canonical SHA-256 of a migration's SQL text.
// Normalisation rules (per SOLO-08 §10):
//   - Strip UTF-8 BOM (0xEF 0xBB 0xBF) if present
//   - Normalise line endings to LF (\n)
//   - Ensure exactly one trailing newline
func ComputeMigrationSHA(sqlText string) string {
	// Strip BOM.
	sqlText = strings.TrimPrefix(sqlText, "\xef\xbb\xbf")
	// Normalise line endings.
	sqlText = strings.ReplaceAll(sqlText, "\r\n", "\n")
	sqlText = strings.ReplaceAll(sqlText, "\r", "\n")
	// Ensure single trailing newline.
	sqlText = strings.TrimRight(sqlText, "\n") + "\n"
	h := sha256.Sum256([]byte(sqlText))
	return fmt.Sprintf("%x", h)
}

// ensureMigrationsTable creates schema_migrations if it does not exist.
func ensureMigrationsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version       INTEGER PRIMARY KEY,
		applied_at    INTEGER NOT NULL,
		binary_sha    TEXT NOT NULL,
		migration_sha TEXT NOT NULL,
		applied_by    TEXT NOT NULL
	)`)
	return err
}

// currentVersion returns the maximum applied migration version, or 0 if none.
func currentVersion(db *sql.DB) (int, error) {
	var v sql.NullInt64
	err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("query current version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// maxVersion returns the highest migration version in the registry.
func maxVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// MigrationCount returns the number of migrations in the embedded registry.
// Exported primarily for testing idempotency assertions.
func MigrationCount() int {
	return len(migrations)
}

// verifyAppliedSHAs checks that every already-applied migration's recorded
// migration_sha still matches the embedded SQL text.  Returns an error
// describing any mismatch (tamper detection per SOLO-08 §10).
func verifyAppliedSHAs(db *sql.DB) error {
	rows, err := db.Query(`SELECT version, migration_sha FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("query applied SHAs: %w", err)
	}
	defer rows.Close()

	// Build a fast lookup: version → embedded SHA.
	embeddedSHA := make(map[int]string, len(migrations))
	for _, m := range migrations {
		embeddedSHA[m.version] = ComputeMigrationSHA(m.sql)
	}

	for rows.Next() {
		var version int
		var recordedSHA string
		if err := rows.Scan(&version, &recordedSHA); err != nil {
			return fmt.Errorf("scan migration row: %w", err)
		}
		expected, ok := embeddedSHA[version]
		if !ok {
			// Migration version recorded in DB but not in binary — too new.
			return fmt.Errorf("migration version %d is recorded in DB but unknown to this binary", version)
		}
		if recordedSHA != expected {
			return fmt.Errorf("migration %d tamper detected: recorded SHA %s != expected %s",
				version, recordedSHA, expected)
		}
	}
	return rows.Err()
}

// snapshotPath builds the auto-snapshot file path.
// Format: <backupDir>/<from>-<to>-<timestamp>.db
// The timestamp includes nanoseconds to avoid collisions when two processes
// snapshot the same DB in the same second (e.g. parallel tests).
func snapshotPath(backupDir string, from, to int) string {
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%d-%d-%s.db", from, to, ts)
	return filepath.Join(backupDir, name)
}

// runMigrations applies all pending migrations against db.
// backupDir is used for the pre-migration snapshot.
// appliedBy is recorded in the schema_migrations table.
func runMigrations(db *sql.DB, current, target int, backupDir, appliedBy string) error {
	if current == target {
		return nil
	}

	// Collect pending migrations.
	var pending []migration
	for _, m := range migrations {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	// Auto-snapshot before first migration.
	snapPath := snapshotPath(backupDir, current, target)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return fmt.Errorf("create backup dir %s: %w", backupDir, err)
	}
	if err := VacuumInto(context.Background(), db, snapPath); err != nil {
		return fmt.Errorf("auto-snapshot failed (refusing to migrate): %w", err)
	}

	// Apply each migration in its own transaction.
	binarySHA := computeBinarySHA()
	for _, m := range pending {
		migSHA := ComputeMigrationSHA(m.sql)
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %d: %w (pre-migration snapshot: %s)", m.version, err, snapPath)
		}

		// Execute the migration SQL — may contain multiple statements.
		if _, err := tx.Exec(m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d failed (snapshot at %s): %w", m.version, snapPath, err)
		}

		// Record in schema_migrations.
		_, err = tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at, binary_sha, migration_sha, applied_by) VALUES (?, ?, ?, ?, ?)`,
			m.version, time.Now().Unix(), binarySHA, migSHA, appliedBy,
		)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d (snapshot at %s): %w", m.version, snapPath, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d (snapshot at %s): %w", m.version, snapPath, err)
		}
	}
	return nil
}

// computeBinarySHA returns a short SHA-256 of the running binary's path for traceability.
// Falls back to "unknown" if the executable path cannot be determined.
func computeBinarySHA() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		// In tests the binary may not be readable; use the path as a stand-in.
		h := sha256.Sum256([]byte(exe))
		return fmt.Sprintf("%x", h[:8])
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}

// CheckMigrationIntegrity opens the DB at path and verifies the recorded
// migration SHAs match the embedded SQL.  Returns a non-nil error on mismatch.
// This is exported primarily for testing.
func CheckMigrationIntegrity(path string) error {
	db, err := sql.Open(sqldriver.Name, path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	return verifyAppliedSHAs(db)
}

// defaultBackupDir returns the default auto-snapshot directory.
// Path: $VESKA_HOME/backups/.pre-migration/ (solov2-n57f co-located with
// user-initiated backups so a single `rm -rf $VESKA_HOME` clears them
// too). Falls back to ~/.veska-backups/.pre-migration when VESKA_HOME is
// unset and the user's home dir cannot be determined.
func defaultBackupDir() string {
	if dir := os.Getenv("VESKA_HOME"); dir != "" {
		return filepath.Join(dir, "backups", ".pre-migration")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".veska", "backups", ".pre-migration")
	}
	return filepath.Join(home, ".veska", "backups", ".pre-migration")
}

// normaliseDSN returns a file: DSN for the given path with the build-time
// driver's standard pragmas (WAL + foreign_keys + synchronous=NORMAL +
// busy_timeout). Delegates to sqldriver so the URI parameter format matches
// whichever driver is compiled in.
func normaliseDSN(path string) string {
	return sqldriver.BuildDSN(path, 5000)
}

// applyPragmas sets WAL mode, autocheckpoint, foreign-keys, and a 5s
// busy_timeout on db. Foreign-keys are also set via the DSN; this
// ensures enforcement on every connection.
//
// The busy_timeout matches the daemon's read+writeHot pools (pools.go).
// Without it, any CLI command opening the same db while the daemon is
// writing (e.g. `veska reindex` racing the embedder worker) fails
// immediately with SQLITE_BUSY. The 5s ceiling is long enough to ride
// out the embedder's batch commits but short enough that a wedged
// daemon surfaces as a clear error rather than an indefinite hang.
func applyPragmas(db *sql.DB) error {
	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA wal_autocheckpoint=1000`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("exec %q: %w", p, err)
		}
	}
	return nil
}

// openAndMigrate is the shared implementation for Open and OpenWithOptions.
func openAndMigrate(path, backupDir, appliedBy string) (*sql.DB, error) {
	db, err := sql.Open(sqldriver.Name, normaliseDSN(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open %s: %w", path, err)
	}

	// Single-writer: serialise all writes through one connection.
	db.SetMaxOpenConns(1)

	// Set WAL mode and autocheckpoint before any migration work.
	if err := applyPragmas(db); err != nil {
		db.Close()
		return nil, err
	}

	if err := ensureMigrationsTable(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure migrations table: %w", err)
	}

	current, err := currentVersion(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	target := maxVersion()

	// Behaviour matrix (SOLO-08 §10.2).
	switch {
	case current == target:
		// Up to date — verify SHAs, then return.
		if err := verifyAppliedSHAs(db); err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "veska: migration integrity check failed: %v\n", err)
			os.Exit(78)
		}
		return db, nil

	case current > target:
		db.Close()
		fmt.Fprintf(os.Stderr, "veska: DB schema version %d is newer than binary max %d; refusing to start\n", current, target)
		os.Exit(78)

	case current < minSchemaVersion && current != 0:
		db.Close()
		fmt.Fprintf(os.Stderr, "veska: DB schema version %d is below minimum supported %d; refusing to start\n", current, minSchemaVersion)
		os.Exit(78)

	default:
		// current < target (and either 0 or within range): apply pending migrations.
		// Verify already-applied SHAs before running new ones.
		if current > 0 {
			if err := verifyAppliedSHAs(db); err != nil {
				db.Close()
				fmt.Fprintf(os.Stderr, "veska: migration integrity check failed before upgrade: %v\n", err)
				os.Exit(78)
			}
		}
		if err := runMigrations(db, current, target, backupDir, appliedBy); err != nil {
			db.Close()
			fmt.Fprintf(os.Stderr, "veska: migration failed: %v\n", err)
			os.Exit(78)
		}
	}

	return db, nil
}

// Options controls optional behaviour for OpenWithOptions.
type Options struct {
	// BackupDir overrides the default auto-snapshot directory.
	// Defaults to ~/.veska-backups/.pre-migration/.
	BackupDir string

	// AppliedBy is recorded in schema_migrations.applied_by.
	// Defaults to the running executable path.
	AppliedBy string
}

// OpenWithOptions opens (or creates) the SQLite database at path, applies
// pending migrations, and returns a ready-to-use *sql.DB.  It accepts an
// Options struct for override of backup directory and applied_by label.
//
// This function intentionally calls os.Exit(78) on schema mismatch, tamper
// detection, or migration failure — that is the specified contract.
func OpenWithOptions(path string, opts Options) (*sql.DB, error) {
	backupDir := opts.BackupDir
	if backupDir == "" {
		backupDir = defaultBackupDir()
	}
	appliedBy := opts.AppliedBy
	if appliedBy == "" {
		if exe, err := os.Executable(); err == nil {
			appliedBy = exe
		} else {
			appliedBy = "unknown"
		}
	}
	return openAndMigrate(path, backupDir, appliedBy)
}

// Open opens (or creates) the SQLite database at path, applies pending
// migrations, and returns a ready-to-use *sql.DB.  WAL mode and
// wal_autocheckpoint=1000 are set unconditionally.
//
// Exit 78 semantics per SOLO-08 §10.2: schema too old, too new, SHA tamper, or
// migration failure all cause os.Exit(78) after writing a message to stderr.
func Open(path string) (*sql.DB, error) {
	return OpenWithOptions(path, Options{})
}
