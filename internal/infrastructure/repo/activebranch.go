package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// SetActiveBranch updates repos.active_branch for the given repoID.
// Returns nil if repoID is not found — unregistered repos are a silent no-op
// so the post-checkout hook never blocks a checkout.
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
