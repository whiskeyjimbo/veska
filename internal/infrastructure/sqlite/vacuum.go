// Package sqlite provides the SQLite substrate for veska.
//
// It opens the database with WAL mode, runs the migration runner defined in
// migrations.go, and exposes VacuumInto for pre-migration snapshots.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// VacuumInto creates a compact copy of db at destPath using SQLite's VACUUM INTO
// statement.  The destination must not already exist; the caller is responsible for
// creating the parent directory.
//
// The context is checked before the VACUUM is executed, so a cancelled context
// returns an error without touching destPath.
func VacuumInto(ctx context.Context, db *sql.DB, destPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sqlite.VacuumInto: context cancelled before snapshot: %w", err)
	}

	// Refuse to overwrite an existing file — a pre-migration snapshot must be unique.
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("sqlite.VacuumInto: destination already exists: %s", destPath)
	}

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		// Best-effort cleanup of a partially written file.
		_ = os.Remove(destPath)
		return fmt.Errorf("sqlite.VacuumInto: %w", err)
	}
	return nil
}
