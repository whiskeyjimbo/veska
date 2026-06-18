// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo

// Package repo provides Git working-tree detection, canonicalization, and repository ID derivation helpers.

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

// detectActiveBranch reads the current branch from a Git working tree. It returns an empty string
// if the directory is not a Git repository, is in a detached HEAD state, or Git is unavailable.
func detectActiveBranch(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "symbolic-ref", "--short", "-q", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// CurrentBranch returns the name of the currently checked-out branch at the repository root path.
// It returns an empty string and no error if the directory is missing, not a Git repository,
// or in a detached HEAD state.
func CurrentBranch(root string) (string, error) {
	return detectActiveBranch(context.Background(), root), nil
}

// detectOriginURL reads the Git remote origin URL and returns its canonicalized HTTPS form.
// It returns an empty string if origin is missing or cannot be canonicalized.
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

// hashAnchor returns the SHA-256 hash of a string, used uniformly across all repository identity tiers.
func hashAnchor(anchor string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s", anchor)
	return hex.EncodeToString(h.Sum(nil))
}

// repoID returns a deterministic hexadecimal repository ID derived from the canonicalized repository path.
func repoID(canonicalPath string) string {
	return hashAnchor(canonicalPath)
}

// IdentityTier identifies which fallback step was used to establish repository identity.
type IdentityTier string

const (
	// TierModuleHostPath represents an in-tree module path structured as a canonical host/path.
	TierModuleHostPath IdentityTier = "module-hostpath"
	// TierOriginURL represents the canonicalized Git remote origin URL.
	TierOriginURL IdentityTier = "origin-url"
	// TierModuleBare represents a vanity or bare module name lacking host structure.
	TierModuleBare IdentityTier = "module-bare"
	// TierAbsRoot represents the absolute root path of the local working directory.
	TierAbsRoot IdentityTier = "abs-root"
)

// Converges reports whether the identity tier is globally unique and stable enough
// to be shared across multiple environments or contributors.
func (t IdentityTier) Converges() bool { return t == TierModuleHostPath }

// ResolveIdentity selects the strongest available identity tier for the repository path
// by evaluating module host/paths, origin URLs, bare module names, or absolute directory roots.
func ResolveIdentity(ctx context.Context, canonical string) (IdentityTier, string, string) {
	mod := readModulePath(canonical)
	if anchor, ok := moduleHostPathAnchor(mod); ok {
		return TierModuleHostPath, anchor, hashAnchor(anchor)
	}
	if origin := detectOriginURL(ctx, canonical); origin != "" {
		return TierOriginURL, origin, hashAnchor(origin)
	}
	if mod != "" {
		return TierModuleBare, mod, hashAnchor(mod)
	}
	return TierAbsRoot, canonical, hashAnchor(canonical)
}

// moduleHostPathAnchor returns true if the module path conforms to Go dns-like modules or npm scoped packages.
func moduleHostPathAnchor(mod string) (string, bool) {
	if mod == "" {
		return "", false
	}
	if strings.HasPrefix(mod, "@") && strings.Contains(mod, "/") {
		return mod, true // scoped npm
	}
	first, rest, ok := strings.Cut(mod, "/")
	if !ok || rest == "" || !strings.Contains(first, ".") {
		return "", false
	}
	return mod, true // host/path-shaped Go module
}

// RepoIDForPath resolves symlinks and obtains the absolute path to generate a stable repository ID.
func RepoIDForPath(path string) string {
	canonical, err := canonicalise(path)
	if err != nil {
		canonical = path
	}
	return repoID(canonical)
}

// DerivedRepoIDFromURL generates a deterministic ID for a repository based on its canonicalized URL.
func DerivedRepoIDFromURL(canonicalURL string) string {
	return hashAnchor(canonicalURL)
}

// validateRepoRoot ensures the given path exists, is a directory, and resides within a Git working tree.
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
