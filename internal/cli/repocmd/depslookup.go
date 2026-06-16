package repocmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// FindTrackedRepoByModulePath returns the short_id of any registered tracked
// (non-synthetic) repo whose module_path equals modulePath, or "" when no such
// repo exists. Used by `veska deps index` to refuse indexing a vendored copy of
// an already-tracked module.
// The query lives here rather than in cmd/veska so the delivery layer holds no
// SQL — it is the deps-index sibling of the resolution helpers above.
func FindTrackedRepoByModulePath(ctx context.Context, db *sql.DB, modulePath string) (string, error) {
	if modulePath == "" {
		return "", nil
	}
	const q = `SELECT repo_id FROM repos
		WHERE module_path = ? AND kind = 'tracked' AND repo_id NOT LIKE 'ext:%'
		LIMIT 1`
	var rid string
	err := db.QueryRowContext(ctx, q, modulePath).Scan(&rid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find tracked repo by module_path: %w", err)
	}
	return ShortRepoID(rid), nil
}

// LookupRepoRootAndBranch is a thin direct-DB lookup used by `veska deps index`
// when the daemon is offline (and so eng_get_repo isn't available). Accepts
// everything ResolveCLIRepoID accepts: full repo_id, 12-char short_id, user
// alias, or unambiguous prefix. Returns the canonical root path +
// active branch for the resolved repo, defaulting the branch to "main".
func LookupRepoRootAndBranch(ctx context.Context, db *sql.DB, repoID string) (root, branch string, err error) {
	recs, err := repo.List(ctx, db)
	if err != nil {
		return "", "", fmt.Errorf("list repos: %w", err)
	}
	rec, err := ResolveCLIRepoID(recs, repoID)
	if err != nil {
		return "", "", err
	}
	branch = rec.ActiveBranch
	if branch == "" {
		branch = "main"
	}
	return rec.RootPath, branch, nil
}
