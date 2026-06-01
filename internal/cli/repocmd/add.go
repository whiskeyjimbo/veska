package repocmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// LooksLikeRepoURL reports whether arg should be treated as a remote git URL
// by `veska repo add`. A literal filesystem path starting with '/', './',
// '~/' or an existing path on disk is always treated as a path; anything
// else that parses cleanly via repo.CanonicalURL is a URL.
func LooksLikeRepoURL(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || strings.HasPrefix(arg, "~") {
		return false
	}
	if _, err := os.Stat(arg); err == nil {
		return false
	}
	_, err := repo.CanonicalURL(arg)
	return err == nil
}

// RunRepoAddPath implements `veska repo add <path>`: resolve the path to
// absolute form, register via the daemon when up (cold scan + live watch),
// and fall back to a direct SQLite write when the daemon is unreachable.
func RunRepoAddPath(ctx context.Context, w io.Writer, root string, wait bool) error {
	// solov2-clgn: '.', '..', or any relative path must be resolved against
	// the user's cwd here. The daemon's cwd is unrelated and would otherwise
	// mis-resolve '.' to the daemon's working dir.
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("repo add: resolve %q: %w", root, err)
	}
	root = abs

	// Prefer the daemon when up — it triggers cold scan and seeds the live
	// watcher in one call (parity with eng_add_repo).
	id, existed, dialErr := dialAddRepo(ctx, root)
	if dialErr == nil {
		return reportDaemonAdd(ctx, w, daemonAddResult{ID: id, Root: root, Existed: existed}, wait)
	}

	// solov2-qnt9: --wait promises to block until cold scan completes; when
	// the daemon is unreachable there IS no cold scan to wait on, so silently
	// degrading to the direct-write fallback would make the flag a lie.
	if wait {
		return fmt.Errorf("repo add --wait: daemon unreachable (%v); start it with `veska service start` and re-run, or drop --wait to register the repo offline (next daemon start will cold-scan it)", dialErr)
	}
	return directAdd(ctx, w, root, dialErr)
}

// daemonAddResult is the outcome of a daemon-path `eng_add_repo`: the assigned
// repo id, the resolved root, and whether the repo was already registered.
type daemonAddResult struct {
	ID      string
	Root    string
	Existed bool
}

// reportDaemonAdd prints the daemon-path add result and either blocks on the
// cold scan (--wait) or prints the background-scan hint.
func reportDaemonAdd(ctx context.Context, w io.Writer, res daemonAddResult, wait bool) error {
	if res.Existed {
		fmt.Fprintf(w, "repo already registered: %s (via daemon)\n", ShortRepoID(res.ID))
		return nil
	}
	fmt.Fprintf(w, "added repo %s (via daemon)\n", ShortRepoID(res.ID))
	if !wait {
		promptAliasAfterAdd(ctx, w, res.ID, "", res.Root)
		fmt.Fprintln(w, ColdScanRunningHint(res.Root, daemonLogPath()))
		return nil
	}
	return WaitForScanComplete(ctx, w, res.ID)
}

// directAdd inserts the repo row + installs hooks without the daemon. The next
// daemon start cold-scans it via StartupResync. Surfaces the dial error so the
// user can tell 'daemon down' from 'daemon up but unreachable' .
func directAdd(ctx context.Context, w io.Writer, root string, dialErr error) error {
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return fmt.Errorf("repo add: %w", err)
	}
	defer closeFn()

	id, existedLocal, err := repo.Add(ctx, db, root)
	if err != nil {
		return fmt.Errorf("repo add: %w", err)
	}
	verb := "added"
	if existedLocal {
		verb = "already registered"
	}
	fmt.Fprintf(w, "%s repo %s (direct write; daemon dial failed: %v — restart daemon to cold-scan/live-watch)\n", verb, ShortRepoID(id), dialErr)
	if !existedLocal {
		promptAliasAfterAdd(ctx, w, id, "", root)
	}
	return nil
}

// promptAliasAfterAdd is the post-`repo add` auto-suggest helper. Opens a
// transient DB handle, asks the user if they want to alias the freshly
// registered repo, and writes the binding . Best-effort —
// errors are logged and swallowed so a prompt failure never breaks the
// add flow itself.
func promptAliasAfterAdd(ctx context.Context, w io.Writer, repoID, canonicalURL, rootPath string) {
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return
	}
	defer closeFn()
	if err := RunAliasSuggestPrompt(ctx, db, AliasTarget{RepoID: repoID, CanonicalURL: canonicalURL, RootPath: rootPath}, DefaultPromptDeps(w)); err != nil {
		fmt.Fprintf(w, "alias prompt: %v\n", err)
	}
}

// RunRepoAddURL implements `veska repo add <url>`: canonicalise the URL,
// short-circuit on a matching canonical_url row, clone to the tracked tier
// with live progress, then register via the daemon (with direct fallback)
// and stamp canonical_url on the new row (solov2-kxo5.3).
//
// Errors during register-or-canonical_url-update roll the clone back so a
// retry starts clean and no orphan directory pretends to be a repo.
func RunRepoAddURL(ctx context.Context, w, stderr io.Writer, rawURL string, wait bool) error {
	canonical, err := repo.CanonicalURL(rawURL)
	if err != nil {
		return fmt.Errorf("repo add: %w", err)
	}
	if done, err := shortCircuitRegisteredURL(ctx, w, canonical); done || err != nil {
		return err
	}

	dest, err := cloneTrackedURL(ctx, w, stderr, canonical)
	if err != nil {
		return err
	}

	id, existed, via, err := registerClonedRepo(ctx, dest, wait)
	if err != nil {
		return err
	}

	db, closeFn, err := OpenLocalDB()
	if err != nil {
		_ = os.RemoveAll(dest)
		return fmt.Errorf("repo add: %w", err)
	}
	defer closeFn()
	// Stamp canonical_url on the freshly-registered row. Failure here rolls
	// back both the row and the clone — a row without canonical_url looks like
	// a normal path-registered repo and would confuse alias-resolution.
	if err := repo.SetCanonicalURL(ctx, db, id, canonical); err != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM repos WHERE repo_id = ?`, id)
		_ = os.RemoveAll(dest)
		return fmt.Errorf("repo add: %w", err)
	}

	verb := "added"
	if existed {
		verb = "already registered"
	}
	fmt.Fprintf(w, "%s repo %s (%s)\n", verb, ShortRepoID(id), via)

	if !existed && !wait {
		// db is already open here; reuse it. The prompt is TTY-only and
		// swallows its own errors.
		if err := RunAliasSuggestPrompt(ctx, db, AliasTarget{RepoID: id, CanonicalURL: canonical, RootPath: dest}, DefaultPromptDeps(w)); err != nil {
			fmt.Fprintf(w, "alias prompt: %v\n", err)
		}
	}
	if wait {
		return WaitForScanComplete(ctx, w, id)
	}
	if !existed {
		fmt.Fprintln(w, ColdScanRunningHint(dest, daemonLogPath()))
	}
	return nil
}

// shortCircuitRegisteredURL prints + returns done=true when canonical is
// already registered. Opens and closes the DB handle for this read so the WAL
// lock is released before the network-bound clone.
func shortCircuitRegisteredURL(ctx context.Context, w io.Writer, canonical string) (done bool, err error) {
	db, closeFn, openErr := OpenLocalDB()
	if openErr != nil {
		return false, nil // best-effort short-circuit; fall through to clone
	}
	existing, ok, lookupErr := repo.LookupByCanonicalURL(ctx, db, canonical)
	closeFn()
	if lookupErr != nil {
		return false, fmt.Errorf("repo add: %w", lookupErr)
	}
	if ok {
		fmt.Fprintf(w, "repo already registered: %s (%s)\n", ShortRepoID(existing.RepoID), existing.RootPath)
		return true, nil
	}
	return false, nil
}

// cloneTrackedURL clones canonical into the tracked-tier path, clearing any
// stale fragment from a prior failed clone first. The clone is rolled back on
// failure so a retry starts clean.
func cloneTrackedURL(ctx context.Context, w, stderr io.Writer, canonical string) (string, error) {
	dest := repo.TrackedClonePath(config.DefaultVectorDir(), canonical)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", fmt.Errorf("repo add: %w", err)
	}
	_ = os.RemoveAll(dest)
	fmt.Fprintf(w, "cloning %s\n", canonical)
	if _, err := repo.Clone(ctx, canonical, dest, stderr); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("repo add: %w", err)
	}
	return dest, nil
}

// registerClonedRepo registers dest via the daemon when up, falling back to a
// direct write. via describes the path taken for the user-facing summary. The
// clone is rolled back on any registration failure.
func registerClonedRepo(ctx context.Context, dest string, wait bool) (id string, existed bool, via string, err error) {
	id, existed, dialErr := dialAddRepo(ctx, dest)
	if dialErr == nil {
		return id, existed, "via daemon", nil
	}
	if wait {
		_ = os.RemoveAll(dest)
		return "", false, "", fmt.Errorf("repo add --wait: daemon unreachable (%v); start it with `veska service start` and re-run, or drop --wait", dialErr)
	}
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		_ = os.RemoveAll(dest)
		return "", false, "", fmt.Errorf("repo add: %w", err)
	}
	defer closeFn()
	id, existed, addErr := repo.Add(ctx, db, dest)
	if addErr != nil {
		_ = os.RemoveAll(dest)
		return "", false, "", fmt.Errorf("repo add: %w", addErr)
	}
	return id, existed, fmt.Sprintf("direct write; daemon dial failed: %v", dialErr), nil
}
