// Package repo manages the set of git repositories tracked by Veska.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hookNames lists the Git hooks that Veska installs.
var hookNames = []string{"post-commit", "post-checkout"}

// execWithBusyRetry runs a database query, retrying on SQLITE_BUSY a bounded number of times
// to handle temporary write lock contention during long-running background tasks.
func execWithBusyRetry(ctx context.Context, db *sql.DB, attempts int, delay time.Duration, query string, args ...any) (sql.Result, error) {
	var lastErr error
	for i := range attempts {
		res, err := db.ExecContext(ctx, query, args...)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isSQLiteBusy(err) {
			return nil, err
		}
		if i < attempts-1 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, lastErr
}

// isSQLiteBusy reports whether the error indicates SQLITE_BUSY lock contention, using string matching to remain database driver agnostic.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// veskaBinary resolves the absolute path of the 'veska' CLI binary, stripping daemon/mcp suffixes from the active binary's path if necessary, to ensure git hooks invoke the correct executable.
func veskaBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "veska"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return resolveVeskaBinary(exe)
}

// resolveVeskaBinary maps the current executable path to the path of the sibling CLI binary.
func resolveVeskaBinary(exe string) string {
	dir, base := filepath.Split(exe)
	cliName := base
	switch {
	case strings.HasSuffix(base, "-daemon"):
		cliName = strings.TrimSuffix(base, "-daemon")
	case strings.HasSuffix(base, "-mcp"):
		cliName = strings.TrimSuffix(base, "-mcp")
	}
	candidate := filepath.Join(dir, cliName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return exe
}

// hookScript returns the shell script content for the Git hook, embedding the resolved VESKA_HOME and binary paths.
func hookScript(hookName string) string {
	return fmt.Sprintf("#!/bin/sh\nexport VESKA_HOME=%q\nexec %s hook-runner %s \"$@\"\n",
		veskaHome(), veskaBinary(), hookName)
}

// veskaHome retrieves the active Veska home directory from the environment or default user paths.
func veskaHome() string {
	if dir := os.Getenv("VESKA_HOME"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".veska")
	}
	return ".veska"
}

// installHooks writes Git hook scripts into the repository's hooks directory.
func installHooks(root string) error {
	hooksDir := filepath.Join(root, ".git", "hooks")
	if _, err := os.Stat(hooksDir); os.IsNotExist(err) {
		return nil
	}

	for _, name := range hookNames {
		hookPath := filepath.Join(hooksDir, name)
		tmpPath := hookPath + ".tmp"

		if err := os.WriteFile(tmpPath, []byte(hookScript(name)), 0o644); err != nil {
			return fmt.Errorf("write %s.tmp: %w", name, err)
		}
		if err := os.Chmod(tmpPath, 0o755); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("chmod %s.tmp: %w", name, err)
		}
		if err := os.Rename(tmpPath, hookPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("rename %s: %w", name, err)
		}
	}
	return nil
}

// removeHooks deletes installed Git hooks from the repository hooks directory.
func removeHooks(root string) {
	hooksDir := filepath.Join(root, ".git", "hooks")
	for _, name := range hookNames {
		_ = os.Remove(filepath.Join(hooksDir, name))
	}
}

// watchesPerRepoEstimate is the estimated number of inotify watches consumed per tracked repository.
const watchesPerRepoEstimate = 128

// Add registers a directory as a tracked repository. It performs resource budget checks, canonicalizes the path,
// determines repository identity and branch, inserts the record, and installs Git hooks.
func Add(ctx context.Context, db *sql.DB, rootPath string) (string, bool, error) {
	if _, err := CheckInotifyBudget(0, watchesPerRepoEstimate); err != nil {
		return "", false, fmt.Errorf("repo add: %w", err)
	}

	currentRSS, err := CurrentRSS()
	if err != nil {
		return "", false, fmt.Errorf("repo add: read RSS: %w", err)
	}
	projectedRSS, err := ProjectRepoRSS(rootPath)
	if err != nil {
		return "", false, fmt.Errorf("repo add: project RSS: %w", err)
	}
	if err := CheckRSSBudget(currentRSS, projectedRSS, DefaultRSSSoftCap); err != nil {
		return "", false, fmt.Errorf("repo add: %w", err)
	}

	canonical, err := canonicalise(rootPath)
	if err != nil {
		return "", false, err
	}

	// Prevent registration of paths outside of a Git working tree to avoid permanently unindexed entries.
	if err := validateRepoRoot(canonical); err != nil {
		return "", false, fmt.Errorf("repo add: %w", err)
	}

	// Return the existing repository ID if the canonical root is already registered.
	if existingID, found, err := repoIDByRoot(ctx, db, canonical); err != nil {
		return "", false, fmt.Errorf("repo add: lookup existing: %w", err)
	} else if found {
		if err := installHooks(canonical); err != nil {
			return "", false, fmt.Errorf("install hooks: %w", err)
		}
		return existingID, true, nil
	}

	// Resolve repository identity using stable anchors to ensure consistent IDs across environments.
	tier, anchor, id := ResolveIdentity(ctx, canonical)
	modPath := readModulePath(canonical)
	now := time.Now().Unix()

	// Detect the active branch, defaulting to "main" if Git or HEAD detection fails.
	branch := detectActiveBranch(ctx, canonical)
	if branch == "" {
		branch = "main"
	}

	// Execute the repository registration with a retry loop to mitigate transient lock contention.
	res, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path, identity_tier, identity_anchor)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO NOTHING`,
		id, canonical, now, branch,
		sql.NullString{String: modPath, Valid: modPath != ""},
		string(tier), anchor,
	)
	if err != nil {
		return "", false, fmt.Errorf("insert repo: %w", err)
	}
	rows, _ := res.RowsAffected()
	existed := rows == 0

	// Associate the remote origin URL for canonical mapping on new registrations.
	if !existed {
		if origin := detectOriginURL(ctx, canonical); origin != "" {
			if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
				`UPDATE repos SET canonical_url = ? WHERE repo_id = ?`,
				origin, id,
			); err != nil {
				// Ignore errors updating canonical URL aliases to prevent registration failures.
				_ = err
			}
		}
	}

	if err := installHooks(canonical); err != nil {
		return "", false, fmt.Errorf("install hooks: %w", err)
	}

	return id, existed, nil
}

// Record is a registered repository as stored in the repos table.
type Record struct {
	RepoID          string
	RootPath        string
	ActiveBranch    string // may be empty
	LastPromotedSHA string // may be empty
	Kind            string // "tracked" (default) or "ephemeral"
	// Aliases lists user-defined names associated with the repository.
	Aliases []string
}

// List returns all registered repository records sorted by repository ID.
func List(ctx context.Context, db *sql.DB) ([]Record, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT repo_id, root_path, active_branch, last_promoted_sha, kind
		 FROM repos ORDER BY repo_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		var (
			rec     Record
			branch  sql.NullString
			lastSHA sql.NullString
		)
		if err := rows.Scan(&rec.RepoID, &rec.RootPath, &branch, &lastSHA, &rec.Kind); err != nil {
			return nil, fmt.Errorf("scan repo row: %w", err)
		}
		rec.ActiveBranch = branch.String
		rec.LastPromotedSHA = lastSHA.String
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repo rows: %w", err)
	}
	// Retrieve and attach user-defined aliases to each record.
	aliases, err := AliasesByRepoID(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Aliases = aliases[out[i].RepoID]
	}
	return out, nil
}

// repoIDByRoot checks if a repository root path is already registered and returns its ID.
func repoIDByRoot(ctx context.Context, db *sql.DB, canonical string) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx,
		`SELECT repo_id FROM repos WHERE root_path = ?`, canonical,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// Get retrieves a repository record by its ID, returning a zero-value Record and no error if not found.
func Get(ctx context.Context, db *sql.DB, repoID string) (Record, error) {
	var (
		rec     Record
		branch  sql.NullString
		lastSHA sql.NullString
	)
	err := db.QueryRowContext(ctx,
		`SELECT repo_id, root_path, active_branch, last_promoted_sha, kind
		 FROM repos WHERE repo_id = ?`,
		repoID,
	).Scan(&rec.RepoID, &rec.RootPath, &branch, &lastSHA, &rec.Kind)
	if err == sql.ErrNoRows {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("get repo: %w", err)
	}
	rec.ActiveBranch = branch.String
	rec.LastPromotedSHA = lastSHA.String
	aliases, err := AliasesForRepo(ctx, db, rec.RepoID)
	if err != nil {
		return Record{}, err
	}
	rec.Aliases = aliases
	return rec, nil
}

// Remove deletes a repository record by ID or unique prefix, triggering database cascade deletes and removing Git hooks.
func Remove(ctx context.Context, db *sql.DB, repoID string) error {
	canonical, rootPath, found, err := resolveRepoForRemoval(ctx, db, repoID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("repo not found: %s", repoID)
	}

	// Manually clean up un-referenced node embedding records and outbound post-promotion queues.
	if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`DELETE FROM node_embedding_refs WHERE node_id IN
		 (SELECT node_id FROM nodes WHERE repo_id = ?)`, canonical); err != nil {
		_ = err
	}

	// Remove the repository record with busy retries to handle locks from background scans.
	if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`DELETE FROM repos WHERE repo_id = ?`, canonical,
	); err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`DELETE FROM post_promotion_queue WHERE repo_id = ?`, canonical,
	); err != nil {
		_ = err // Unused.
	}

	if rootPath != "" {
		removeHooks(rootPath)
	}
	return nil
}

// resolveRepoForRemoval resolves a full ID or unique short prefix to a canonical repository ID and path.
func resolveRepoForRemoval(ctx context.Context, db *sql.DB, repoID string) (canonical, rootPath string, found bool, err error) {
	// Exact match first.
	err = db.QueryRowContext(ctx,
		`SELECT repo_id, root_path FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&canonical, &rootPath)
	if err == nil {
		return canonical, rootPath, true, nil
	}
	if err != sql.ErrNoRows {
		return "", "", false, fmt.Errorf("lookup repo: %w", err)
	}

	// Unique prefix match.
	rows, qerr := db.QueryContext(ctx,
		`SELECT repo_id, root_path FROM repos WHERE repo_id LIKE ?`, repoID+"%",
	)
	if qerr != nil {
		return "", "", false, fmt.Errorf("lookup repo: %w", qerr)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&canonical, &rootPath); err != nil {
			return "", "", false, fmt.Errorf("lookup repo: %w", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return "", "", false, fmt.Errorf("lookup repo: %w", err)
	}
	switch n {
	case 0:
		return "", "", false, nil
	case 1:
		return canonical, rootPath, true, nil
	default:
		return "", "", false, fmt.Errorf("ambiguous repo id %q matches %d repos", repoID, n)
	}
}
