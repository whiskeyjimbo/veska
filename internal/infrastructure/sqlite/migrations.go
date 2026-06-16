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

// migrations is the ordered registry of all known migrations. New migrations
// must only be appended to maintain ordering.
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
// It normalizes line endings and removes UTF-8 BOM prefixes to ensure
// consistency across systems.
func ComputeMigrationSHA(sqlText string) string {
	sqlText = strings.TrimPrefix(sqlText, "\xef\xbb\xbf")
	sqlText = strings.ReplaceAll(sqlText, "\r\n", "\n")
	sqlText = strings.ReplaceAll(sqlText, "\r", "\n")
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
func MigrationCount() int {
	return len(migrations)
}

// verifyAppliedSHAs checks that already-applied migrations match the recorded
// SHA-256 hashes to detect database schema tampering.
func verifyAppliedSHAs(db *sql.DB) error {
	rows, err := db.Query(`SELECT version, migration_sha FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("query applied SHAs: %w", err)
	}
	defer rows.Close()

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
			return fmt.Errorf("migration version %d is recorded in DB but unknown to this binary", version)
		}
		if recordedSHA != expected {
			return fmt.Errorf("migration %d tamper detected: recorded SHA %s != expected %s",
				version, recordedSHA, expected)
		}
	}
	return rows.Err()
}

// snapshotPath builds the pre-migration snapshot file path. The timestamp
// includes nanoseconds to avoid file name collisions when running tests in
// parallel.
func snapshotPath(backupDir string, from, to int) string {
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	name := fmt.Sprintf("%d-%d-%s.db", from, to, ts)
	return filepath.Join(backupDir, name)
}

// runMigrations applies all pending migrations against db.
func runMigrations(db *sql.DB, current, target int, backupDir, appliedBy string) error {
	if current == target {
		return nil
	}

	var pending []migration
	for _, m := range migrations {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	snapPath := snapshotPath(backupDir, current, target)
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return fmt.Errorf("create backup dir %s: %w", backupDir, err)
	}
	if err := VacuumInto(context.Background(), db, snapPath); err != nil {
		return fmt.Errorf("auto-snapshot failed (refusing to migrate): %w", err)
	}

	binarySHA := computeBinarySHA()
	for _, m := range pending {
		migSHA := ComputeMigrationSHA(m.sql)
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %d: %w (pre-migration snapshot: %s)", m.version, err, snapPath)
		}

		if _, err := tx.Exec(m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d failed (snapshot at %s): %w", m.version, snapPath, err)
		}

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
func computeBinarySHA() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		// Use the executable path as a fallback hash key if the binary itself
		// is unreadable during test runs.
		h := sha256.Sum256([]byte(exe))
		return fmt.Sprintf("%x", h[:8])
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8])
}

// CheckMigrationIntegrity opens the DB at path and verifies the recorded
// migration SHAs match the embedded SQL. Returns a non-nil error on mismatch.
func CheckMigrationIntegrity(path string) error {
	db, err := sql.Open(sqldriver.Name, path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	return verifyAppliedSHAs(db)
}

// defaultBackupDir returns the pre-migration backup directory path, defaulting
// to co-locate with user-initiated backups under the application home directory.
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

// normaliseDSN delegates DSN construction to the compiled sql driver wrapper.
func normaliseDSN(path string) string {
	return sqldriver.BuildDSN(path, 5000)
}

// applyPragmas sets performance and integrity configuration. The 5-second busy
// timeout prevents concurrent CLI invocations from failing with SQLITE_BUSY
// errors during active background writes.
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

	// Use a single connection limit to serialize database writes.
	db.SetMaxOpenConns(1)

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

	switch {
	case current == target:
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
	BackupDir string

	// AppliedBy is recorded in schema_migrations.applied_by.
	AppliedBy string
}

// OpenWithOptions opens the SQLite database and applies pending migrations. In
// accordance with requirements, it calls os.Exit(78) on migration failures or
// schema tamper detection.
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

// Open opens the SQLite database and runs all migrations using default options.
func Open(path string) (*sql.DB, error) {
	return OpenWithOptions(path, Options{})
}
