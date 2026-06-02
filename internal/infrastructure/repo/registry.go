// Package repo manages the set of git repositories tracked by veska.
// Add registers a repository, reads its module path, and installs git hooks.
// Remove deregisters a repository and removes the installed hooks.
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

// hookNames lists the git hooks that veska installs.
var hookNames = []string{"post-commit", "post-checkout"}

// execWithBusyRetry runs an ExecContext, retrying on SQLITE_BUSY a bounded
// number of times with a fixed delay between attempts. SQLite's per-handle
// busy_timeout already absorbs short contention; this loop covers the
// pathological case where a long-running scan/embedder write holds the
// write lock past that ceiling. Non-busy errors return immediately.
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

// isSQLiteBusy reports whether the error is a SQLITE_BUSY-class lock
// contention. String-matching keeps the helper driver-agnostic.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// veskaBinary resolves the absolute path of the 'veska' CLI binary so
// installed hooks invoke it directly instead of relying on $PATH
// . The hook MUST point at the CLI, not the running process —
// when eng_add_repo runs inside veska-daemon, os.Executable() returns the
// daemon's path, which has no 'hook-runner' subcommand. By convention the
// CLI lives alongside the daemon and mcp shim with these names:
//
//	veska         (the CLI)
//	veska-daemon  (the long-running process)
//	veska-mcp     (the stdio shim)
//
// So we resolve the running binary, strip the '-daemon' / '-mcp' suffix
// from its basename, and check that the sibling exists. If anything fails
// we fall back to bare 'veska' — same $PATH-dependent behaviour as before,
// never worse.
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

// resolveVeskaBinary is the pure path-shaping half of veskaBinary, split out
// for testability. Given the absolute path of the running binary, it returns
// the path of the sibling 'veska' CLI when one exists. If the sibling cannot
// be found (split install, unusual layout) it returns exe verbatim — better
// to print 'unknown command hook-runner' from the running binary than to
// silently emit a broken 'veska' on $PATH.
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

// hookScript returns the shell script content for a named hook. The veska
// binary is resolved by absolute path so the hook works regardless of the
// caller's $PATH at commit time . The current VESKA_HOME is
// also baked in  so the hook reaches the right daemon socket
// regardless of the shell environment 'git commit' runs under — users with
// non-default VESKA_HOME rarely export it, and an unset VESKA_HOME would
// route the hook to ~/.veska/cli.sock and silently fail.
func hookScript(hookName string) string {
	return fmt.Sprintf("#!/bin/sh\nexport VESKA_HOME=%q\nexec %s hook-runner %s \"$@\"\n",
		veskaHome(), veskaBinary(), hookName)
}

// veskaHome returns the resolved VESKA_HOME for baking into hook scripts.
// We don't import internal/config here (repo is lower in the layer chain),
// so this mirrors config.veskaHome's logic locally. Kept small and obvious;
// drift between the two would surface in the integration tests.
func veskaHome() string {
	if dir := os.Getenv("VESKA_HOME"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".veska")
	}
	return ".veska"
}

// installHooks writes veska hook scripts into <root>/.git/hooks/ atomically.
// It is a no-op if the hooks directory does not exist.
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

// removeHooks deletes veska hook scripts from <root>/.git/hooks/ if they exist.
func removeHooks(root string) {
	hooksDir := filepath.Join(root, ".git", "hooks")
	for _, name := range hookNames {
		_ = os.Remove(filepath.Join(hooksDir, name))
	}
}

// watchesPerRepoEstimate is the estimated number of inotify watches consumed
// per tracked repository. Used for the Linux inotify budget check.
const watchesPerRepoEstimate = 128

// Add registers root as a tracked repository. It:
//  1. Checks the Linux inotify watch budget (no-op on other platforms).
//  2. Checks the global RSS soft cap; refuses if projected steady-state exceeds 2 GiB.
//  3. Canonicalises the path and generates a sha256 repo_id.
//  4. Reads the module path from go.mod or package.json.
//  5. Inserts the row into the repos table (idempotent: ON CONFLICT DO NOTHING).
//  6. Installs git hooks.
//
// Returns the repo_id string and a flag indicating whether the row was
// already present (idempotent re-add): existed=true means the INSERT was a
// no-op so callers can surface 'already registered' instead of a misleading
// 'added' message .
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

	// Refuse to register a path that is not inside a git work-tree
	// . Without this check `veska repo add /tmp` silently
	// succeeded, the cold scan failed (no commits / nothing to parse), and
	// the repos table held a permanently-unindexed entry that pinned
	// `doctor status` to "degraded" until the user noticed and manually
	// removed it.
	if err := validateRepoRoot(canonical); err != nil {
		return "", false, fmt.Errorf("repo add: %w", err)
	}

	id := repoID(canonical)
	modPath := readModulePath(canonical)
	now := time.Now().Unix()

	// Detect the current branch from the working tree . Without
	// this every downstream write (Ingester.Save, Promoter.Promote, FTS, vec)
	// is keyed by branch="" and every query API rejects "branch is required"
	// — i.e. a silently-unqueryable graph. Default to "main" when detection
	// fails (no git, detached HEAD, freshly-init'd repo with no commits) so
	// the rest of the pipeline has a usable key.
	branch := detectActiveBranch(ctx, canonical)
	if branch == "" {
		branch = "main"
	}

	// solov2-6c04: a concurrent cold-scan can hold the Write lock long
	// enough to outlast SQLite's busy_timeout (5s), surfacing SQLITE_BUSY
	// to the user even though the INSERT itself is trivial. Wrap the
	// single statement in a short app-level retry loop so the natural
	// "register a second repo while the first is scanning" flow never
	// fails on transient contention. 5 attempts × 500ms = 2.5s additional
	// budget on top of the per-attempt busy_timeout.
	res, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO NOTHING`,
		id, canonical, now, branch,
		sql.NullString{String: modPath, Valid: modPath != ""},
	)
	if err != nil {
		return "", false, fmt.Errorf("insert repo: %w", err)
	}
	rows, _ := res.RowsAffected()
	existed := rows == 0

	// Stamp canonical_url from `git remote get-url origin` for fresh
	// registrations (solov2-kxo5.4). Lets a later `search --repo <url>`
	// against the same repo resolve to this row via LookupByCanonicalURL
	// instead of cloning a duplicate. Skipped for re-adds: per design we
	// do not backfill existing rows. A missing/malformed origin is silent
	// — canonical_url stays NULL and the row behaves like any other.
	if !existed {
		if origin := detectOriginURL(ctx, canonical); origin != "" {
			if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
				`UPDATE repos SET canonical_url = ? WHERE repo_id = ?`,
				origin, id,
			); err != nil {
				// solov2-kxo5.4: the canonical_url is an alias — its
				// absence only forfeits the URL-collision short-circuit,
				// it does not break registration. Log via the returned
				// error would be too loud; swallow and move on so
				// `repo add` itself can't fail on this.
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
	Kind            string // "tracked" (default) or "ephemeral" (solov2-kxo5.9)
	// Aliases are user-defined human-friendly names for this repo
	// . Sorted; nil when no aliases exist.
	Aliases []string
}

// List returns every registered repository ordered by repo_id. The nullable
// active_branch and last_promoted_sha columns are flattened to "". An empty
// repos table yields a nil slice and a nil error.
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
	// solov2-7w1t: decorate every record with its user-defined aliases.
	aliases, err := AliasesByRepoID(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Aliases = aliases[out[i].RepoID]
	}
	return out, nil
}

// Get returns the Record for repoID. A missing row yields (zero Record, nil)
// so callers can distinguish a query error from a not-found row by checking
// the returned RepoID; both nullable columns are flattened to "" exactly as
// List does.
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

// Remove deletes the repo row identified by repoID (CASCADE removes nodes/edges)
// and removes installed git hooks if the git dir still exists.
//
// repoID may be the full id or a unique short prefix (as printed by
// `veska repo add` / `veska repo list`). Without prefix resolution a short id
// matched nothing and the DELETE silently no-op'd, leaving the repo — and,
// since CASCADE then never ran, its child rows — in place .
func Remove(ctx context.Context, db *sql.DB, repoID string) error {
	canonical, rootPath, found, err := resolveRepoForRemoval(ctx, db, repoID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("repo not found: %s", repoID)
	}

	// solov2-6c04 follow-up: a concurrent promotion/scan can hold the
	// Write lock long enough to outlast SQLite's busy_timeout, surfacing
	// SQLITE_BUSY on user-initiated removes. Same retry envelope as Add.
	if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`DELETE FROM repos WHERE repo_id = ?`, canonical,
	); err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	// solov2-zmzc: post_promotion_queue has no FK to repos, so the CASCADE
	// fan-out skips it. Manually drop any rows targeting the removed repo
	// so they don't sit in 'failed'/'pending' forever, dragging doctor
	// rollups to "degraded". --purge-orphans on the doctor command cleans
	// up rows left by older versions.
	if _, err := execWithBusyRetry(ctx, db, 5, 500*time.Millisecond,
		`DELETE FROM post_promotion_queue WHERE repo_id = ?`, canonical,
	); err != nil {
		// Best-effort: the repo row is already gone, so leaving queue rows
		// behind is recoverable via `veska doctor post_promotion_queue
		// --purge-orphans`. Don't fail the user-facing remove for it.
		_ = err
	}

	if rootPath != "" {
		removeHooks(rootPath)
	}
	return nil
}

// resolveRepoForRemoval maps repoID (full id or unique short prefix) to its
// canonical id and root_path. found is false when nothing matches; an
// ambiguous prefix is an error.
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
