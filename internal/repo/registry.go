// Package repo manages the set of git repositories tracked by veska.
// Add registers a repository, reads its module path, and installs git hooks.
// Remove deregisters a repository and removes the installed hooks.
package repo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hookNames lists the git hooks that veska installs.
var hookNames = []string{"post-commit", "post-checkout"}

// veskaBinary resolves the absolute path of the 'veska' CLI binary so
// installed hooks invoke it directly instead of relying on $PATH
// (solov2-v7q). The hook MUST point at the CLI, not the running process —
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
// caller's $PATH at commit time (solov2-v7q). The current VESKA_HOME is
// also baked in (solov2-g50) so the hook reaches the right daemon socket
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

// detectActiveBranch reads the current branch from a working tree via
// 'git symbolic-ref --short HEAD'. Returns "" when the directory is not a
// git working tree, is in a detached-HEAD state, or git is unavailable —
// the caller decides how to handle that (Add defaults to "main"). Bounded
// to a short timeout so a hung git invocation cannot stall registration.
func detectActiveBranch(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "symbolic-ref", "--short", "-q", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// repoID returns a deterministic hex ID for a canonical root path.
func repoID(canonicalPath string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s", canonicalPath)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalise returns the absolute, symlink-resolved path for root.
func canonicalise(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Directory may not exist; fall back to abs.
		return abs, nil
	}
	return resolved, nil
}

// readModulePath attempts to read the module/package name from go.mod or
// package.json in root. Returns "" if neither file exists.
func readModulePath(root string) string {
	// Try go.mod first.
	gomod := filepath.Join(root, "go.mod")
	if f, err := os.Open(gomod); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if after, ok := strings.CutPrefix(line, "module "); ok {
				return strings.TrimSpace(after)
			}
		}
	}

	// Fall back to package.json.
	pkgjson := filepath.Join(root, "package.json")
	if data, err := os.ReadFile(pkgjson); err == nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &pkg); err == nil && pkg.Name != "" {
			return pkg.Name
		}
	}

	return ""
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
// Returns the repo_id string.
func Add(ctx context.Context, db *sql.DB, rootPath string) (string, error) {
	if _, err := CheckInotifyBudget(0, watchesPerRepoEstimate); err != nil {
		return "", fmt.Errorf("repo add: %w", err)
	}

	currentRSS, err := CurrentRSS()
	if err != nil {
		return "", fmt.Errorf("repo add: read RSS: %w", err)
	}
	projectedRSS, err := ProjectRepoRSS(rootPath)
	if err != nil {
		return "", fmt.Errorf("repo add: project RSS: %w", err)
	}
	if err := CheckRSSBudget(currentRSS, projectedRSS, DefaultRSSSoftCap); err != nil {
		return "", fmt.Errorf("repo add: %w", err)
	}

	canonical, err := canonicalise(rootPath)
	if err != nil {
		return "", err
	}

	id := repoID(canonical)
	modPath := readModulePath(canonical)
	now := time.Now().Unix()

	// Detect the current branch from the working tree (solov2-f8p). Without
	// this every downstream write (Ingester.Save, Promoter.Promote, FTS, vec)
	// is keyed by branch="" and every query API rejects "branch is required"
	// — i.e. a silently-unqueryable graph. Default to "main" when detection
	// fails (no git, detached HEAD, freshly-init'd repo with no commits) so
	// the rest of the pipeline has a usable key.
	branch := detectActiveBranch(ctx, canonical)
	if branch == "" {
		branch = "main"
	}

	_, err = db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO NOTHING`,
		id, canonical, now, branch,
		sql.NullString{String: modPath, Valid: modPath != ""},
	)
	if err != nil {
		return "", fmt.Errorf("insert repo: %w", err)
	}

	if err := installHooks(canonical); err != nil {
		return "", fmt.Errorf("install hooks: %w", err)
	}

	return id, nil
}

// Record is a registered repository as stored in the repos table.
type Record struct {
	RepoID          string
	RootPath        string
	ActiveBranch    string // may be empty
	LastPromotedSHA string // may be empty
}

// List returns every registered repository ordered by repo_id. The nullable
// active_branch and last_promoted_sha columns are flattened to "". An empty
// repos table yields a nil slice and a nil error.
func List(ctx context.Context, db *sql.DB) ([]Record, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT repo_id, root_path, active_branch, last_promoted_sha
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
		if err := rows.Scan(&rec.RepoID, &rec.RootPath, &branch, &lastSHA); err != nil {
			return nil, fmt.Errorf("scan repo row: %w", err)
		}
		rec.ActiveBranch = branch.String
		rec.LastPromotedSHA = lastSHA.String
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repo rows: %w", err)
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
		`SELECT repo_id, root_path, active_branch, last_promoted_sha
		 FROM repos WHERE repo_id = ?`,
		repoID,
	).Scan(&rec.RepoID, &rec.RootPath, &branch, &lastSHA)
	if err == sql.ErrNoRows {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("get repo: %w", err)
	}
	rec.ActiveBranch = branch.String
	rec.LastPromotedSHA = lastSHA.String
	return rec, nil
}

// Remove deletes the repo row identified by repoID (CASCADE removes nodes/edges)
// and removes installed git hooks if the git dir still exists.
func Remove(ctx context.Context, db *sql.DB, repoID string) error {
	// Look up root_path before deleting so we can clean up hooks.
	var rootPath string
	err := db.QueryRowContext(ctx,
		`SELECT root_path FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&rootPath)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lookup repo: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`DELETE FROM repos WHERE repo_id = ?`, repoID,
	); err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}

	if rootPath != "" {
		removeHooks(rootPath)
	}
	return nil
}
