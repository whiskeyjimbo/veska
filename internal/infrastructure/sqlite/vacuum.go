// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// VacuumInto creates a compact copy of db at destPath using SQLite's VACUUM INTO
// statement. The destination must not already exist. The caller is responsible for
// creating the parent directory. The context is checked before execution to avoid
// touching the destination path if the operation was canceled.
func VacuumInto(ctx context.Context, db *sql.DB, destPath string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sqlite.VacuumInto: context canceled before snapshot: %w", err)
	}

	// Refuse to overwrite an existing file to ensure the pre-migration snapshot is unique.
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("sqlite.VacuumInto: destination already exists: %s", destPath)
	}

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		// Best-effort cleanup of any partially written destination file.
		_ = os.Remove(destPath)
		return fmt.Errorf("sqlite.VacuumInto: %w", err)
	}
	return nil
}
