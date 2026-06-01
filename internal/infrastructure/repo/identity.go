// Package repo: git working-tree detection, canonicalisation, and repo-ID
// derivation helpers, split out of registry.go to keep that file focused on
// the registry CRUD (Add/List/Get/Remove) and hook lifecycle.
package repo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

// CurrentBranch returns the working-tree current branch name for the repo at
// root via `git symbolic-ref --short HEAD`. It returns "" (and a nil error)
// for detached HEAD / non-git / missing tree — the single source of truth for
// "what branch is checked out", shared by registration and the staging-vs-HEAD
// branch reconcile (SOLO-03 §5.2). The error return exists for symmetry with
// the application.BranchReader port; today every failure flattens to "".
func CurrentBranch(root string) (string, error) {
	return detectActiveBranch(context.Background(), root), nil
}

// detectOriginURL reads `git remote get-url origin` from the working tree
// and returns the canonicalised form (solov2-kxo5.4). Returns "" when the
// remote is missing, git is unavailable, or the URL can't be canonicalised
// — every failure mode is treated identically so `repo add <path>` never
// fails on a remote-config issue that the user doesn't need to care about.
func detectOriginURL(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	canonical, err := CanonicalURL(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return canonical
}

// repoID returns a deterministic hex ID for a canonical root path.
func repoID(canonicalPath string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s", canonicalPath)
	return hex.EncodeToString(h.Sum(nil))
}

// RepoIDForPath returns the deterministic repo_id veska assigns to the repo
// rooted at path. It canonicalises (absolute + symlink-resolved) before
// hashing, exactly as registration does, so out-of-band callers — notably the
// git hook runner, which only has the raw `git rev-parse --show-toplevel`
// output — derive the SAME id the registry stored. Without this, an
// unresolved symlinked checkout path would hash to a different id and updates
// keyed on it (e.g. SetActiveBranch) would silently match zero rows.
func RepoIDForPath(path string) string {
	canonical, err := canonicalise(path)
	if err != nil {
		canonical = path
	}
	return repoID(canonical)
}

// DerivedRepoIDFromURL returns the deterministic hex ID used as repo_id
// for an ephemeral, URL-cloned repository (solov2-kxo5.2). The input must
// already be canonicalised — callers obtain a canonical URL via the
// CanonicalURL helper that lands in kxo5.1. Keeping both id derivations
// (path-based repoID, URL-based DerivedRepoIDFromURL) in the same file
// preserves the invariant that the two id spaces share one hash function.
func DerivedRepoIDFromURL(canonicalURL string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s", canonicalURL)
	return hex.EncodeToString(h.Sum(nil))
}

// validateRepoRoot rejects paths that should never be registered as a
// veska repo:
//   - non-existent paths
//   - non-directory paths
//   - directories with no `.git` entry AND no parent .git work-tree marker
//
// The .git lookup also walks parents so registering a subdirectory of a
// real repo would still be accepted (veska canonicalises to the path the
// user passed; this preserves that behaviour).
func validateRepoRoot(canonical string) error {
	info, err := os.Stat(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("path does not exist: %s", canonical)
		}
		return fmt.Errorf("stat %s: %w", canonical, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", canonical)
	}
	dir := canonical
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("not inside a git work-tree: %s (run `git init` first)", canonical)
		}
		dir = parent
	}
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
