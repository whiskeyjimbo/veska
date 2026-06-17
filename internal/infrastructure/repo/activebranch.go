// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// SetActiveBranch updates the active branch for a repository in the database.
// It returns nil if the repository ID is not found because unregistered
// repositories should be a silent no-op to prevent blocking checkouts during
// post-checkout hooks.
func SetActiveBranch(ctx context.Context, db *sql.DB, repoID, branch string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE repos SET active_branch = ? WHERE repo_id = ?`,
		branch, repoID,
	)
	if err != nil {
		return fmt.Errorf("set active branch: %w", err)
	}
	return nil
}
