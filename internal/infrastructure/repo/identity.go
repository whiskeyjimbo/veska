package repo

// This file holds git working-tree detection, canonicalisation, and repo-ID
// derivation helpers, split out of registry.go to keep that file focused on
// the registry CRUD (Add/List/Get/Remove) and hook lifecycle.

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

// hashAnchor is the single hash function shared by every identity tier
// (ADR-S0017 §2): repo_id is sha256(anchor) regardless of which anchor — module
// path, canonical URL, or absolute root — was chosen. Keeping one function
// guarantees the abs-root tier reproduces the legacy repoID() byte-for-byte, so
// local-only repos see no id churn until the identity-scheme rescan runs.
func hashAnchor(anchor string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s", anchor)
	return hex.EncodeToString(h.Sum(nil))
}

// repoID returns a deterministic hex ID for a canonical root path (the
// abs-root identity tier).
func repoID(canonicalPath string) string {
	return hashAnchor(canonicalPath)
}

// IdentityTier names the rung of the ADR-S0017 fallback chain a repo resolved
// to. Persisted on the repos row (identity_tier) so identity is auditable and
// never silently re-derived per client.
type IdentityTier string

const (
	// TierModuleHostPath — in-tree module path shaped as a canonical host/path
	// (Go `github.com/org/repo`) or scoped npm (`@org/pkg`). Committed content,
	// so byte-identical across clones AND forks, and host/path-shaped, so
	// globally unique: the supported shared-DB anchor.
	TierModuleHostPath IdentityTier = "module-hostpath"
	// TierOriginURL — canonical git remote URL. Globally unique but local git
	// config, so it diverges across forks. Used when no module manifest exists.
	TierOriginURL IdentityTier = "origin-url"
	// TierModuleBare — a bare/vanity module name (`module myapp`) that is not
	// host/path-shaped. Local-stable but collision-prone in a shared DB, so it
	// ranks below any globally-unique anchor.
	TierModuleBare IdentityTier = "module-bare"
	// TierAbsRoot — absolute checkout path. Never converges; local-only
	// fallback reproducing pre-ADR-S0017 behaviour.
	TierAbsRoot IdentityTier = "abs-root"
)

// Converges reports whether this tier can participate in a shared graph DB —
// i.e. whether two contributors indexing the same upstream resolve to the same
// repo_id. Only the host/path module anchor converges AND is globally unique;
// origin-url converges only when origins agree (doctor warns on the rest).
func (t IdentityTier) Converges() bool { return t == TierModuleHostPath }

// ResolveIdentity selects the strongest available identity anchor for the repo
// at canonical (an already-canonicalised absolute root) per the ADR-S0017
// locked chain — host/path module > origin URL > bare module > abs-root — and
// returns the chosen tier, the exact anchor string hashed, and the resulting
// repo_id. Resolved ONCE at repo.Add and persisted; never re-derived per
// client (convergence comes from the anchor being globally shared, not from
// each client recomputing it).
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

// moduleHostPathAnchor reports whether a module path is shaped as a canonical
// host/path (Go convention `github.com/org/repo` — first segment is a DNS host)
// or a scoped npm package (`@org/pkg`). Such a path is simultaneously committed
// content and globally unique, the strongest possible anchor.
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
	return hashAnchor(canonicalURL)
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
