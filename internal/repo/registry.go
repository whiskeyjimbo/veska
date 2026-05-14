// Package repo manages the set of git repositories tracked by engram.
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
	"path/filepath"
	"strings"
	"time"
)

// hookNames lists the git hooks that engram installs.
var hookNames = []string{"post-commit", "post-checkout"}

// hookScript returns the shell script content for a named hook.
func hookScript(hookName string) string {
	return fmt.Sprintf("#!/bin/sh\nexec engram hook-runner %s \"$@\"\n", hookName)
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

// installHooks writes engram hook scripts into <root>/.git/hooks/ atomically.
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

// removeHooks deletes engram hook scripts from <root>/.git/hooks/ if they exist.
func removeHooks(root string) {
	hooksDir := filepath.Join(root, ".git", "hooks")
	for _, name := range hookNames {
		_ = os.Remove(filepath.Join(hooksDir, name))
	}
}

// Add registers root as a tracked repository. It:
//  1. Canonicalises the path and generates a sha256 repo_id.
//  2. Reads the module path from go.mod or package.json.
//  3. Inserts the row into the repos table (idempotent: ON CONFLICT DO NOTHING).
//  4. Installs git hooks.
//
// Returns the repo_id string.
func Add(ctx context.Context, db *sql.DB, rootPath string) (string, error) {
	canonical, err := canonicalise(rootPath)
	if err != nil {
		return "", err
	}

	id := repoID(canonical)
	modPath := readModulePath(canonical)
	now := time.Now().Unix()

	_, err = db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at, module_path)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(repo_id) DO NOTHING`,
		id, canonical, now, sql.NullString{String: modPath, Valid: modPath != ""},
	)
	if err != nil {
		return "", fmt.Errorf("insert repo: %w", err)
	}

	if err := installHooks(canonical); err != nil {
		return "", fmt.Errorf("install hooks: %w", err)
	}

	return id, nil
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
