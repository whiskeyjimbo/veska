// Package reindexcmd holds the business logic behind the `veska reindex`
// command: the daemon-dispatch fork (route through eng_reindex_repo when the
// daemon is up) and the direct-SQLite cold-scan fallback when it is down.
// cmd/veska/reindex.go is reduced to Cobra command construction whose RunE body
// merges the positional/flag target and delegates here, injecting the
// cmd-owned cold-scan seams (ReparserFactory, MatchByPath) it shares with
// `veska search` (, following the cmd = glue / logic-in-packages
// pattern established by searchcmd, symbolcmd, graphcmd, and findingscmd).
package reindexcmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// Params bundles the inputs of Run. The seam fields (DaemonRunning,
// DialReindex, ReparserFactory, MatchByPath) are injected by the Cobra layer so
// tests can substitute spies without standing up a real Unix socket or the
// ingester/promoter/sinks pipeline. ReparserFactory and MatchByPath are the
// same cmd-owned cold-scan seams `veska search` injects.
type Params struct {
	// Target is the merged positional/--repo selector ("" = cwd-resolve).
	Target string
	Out    io.Writer
	ErrOut io.Writer

	// DaemonRunning reports whether the daemon socket is reachable.
	DaemonRunning func() bool
	// DialReindex routes the reindex through the daemon's eng_reindex_repo MCP
	// tool. Either repoID or rootPath may be empty; the handler accepts either.
	DialReindex func(ctx context.Context, repoID, rootPath string) (string, error)
	// ReparserFactory builds a cold-scan reparser closure from an open pool set.
	ReparserFactory func(*sqlite.Pools, application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error)
	// MatchByPath resolves a filesystem path to its registered repo Record.
	MatchByPath func(ctx context.Context, db *sql.DB, path string) (repo.Record, error)
}

// Run performs a full cold-scan reparse of the target (or cwd-resolved) repo
// unconditionally — bypassing the daemon's StartupResync gate that skips
// at-HEAD repos. When the daemon is up the reindex is routed through its
// eng_reindex_repo MCP tool so the user does not have to stop the
// daemon; the direct-SQLite path below handles the no-daemon case.
func Run(ctx context.Context, p Params) error {
	w := p.Out

	// when the daemon is up, route through eng_reindex_repo. The
	// previous behaviour (refuse with a stop-the-daemon hint)
	// disconnected the editor's MCP session and was a junior-hostile regression
	// from add-time scans (which already run inside the daemon).
	if p.DaemonRunning() {
		repoID, rootPath, derr := resolveTargetForDial(ctx, p.Target)
		if derr != nil {
			return derr
		}
		fmt.Fprintf(w, "reindexing via daemon...\n")
		gotID, err := p.DialReindex(ctx, repoID, rootPath)
		if err != nil {
			return fmt.Errorf("reindex: %w", err)
		}
		if gotID == "" {
			gotID = repoID
		}
		fmt.Fprintf(w, "reindex complete: repo %s\n", gotID)
		return nil
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	// Apply migrations so the schema is present even on a freshly created
	// database; mirror the wiki command's belt-and-braces open-twice pattern.
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		return fmt.Errorf("reindex: migrate sqlite: %w", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("reindex: open sqlite pools: %w", err)
	}
	defer func() { _ = pools.Close() }()

	rec, err := resolveReindexTarget(ctx, pools.ReadDB, p.Target, p.MatchByPath)
	if err != nil {
		return err
	}

	loader := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := p.ReparserFactory(pools, loader)
	if err != nil {
		return err
	}

	appRec := application.RepoRecord{
		RepoID:          rec.RepoID,
		RootPath:        rec.RootPath,
		ActiveBranch:    rec.ActiveBranch,
		LastPromotedSHA: rec.LastPromotedSHA,
	}
	if appRec.ActiveBranch == "" {
		appRec.ActiveBranch = "main"
	}

	fmt.Fprintf(w, "reindexing %s at %s...\n", appRec.RepoID, appRec.RootPath)
	if err := reparser(ctx, appRec); err != nil {
		return fmt.Errorf("reindex: %w", err)
	}

	// Best-effort HEAD lookup for the trailing message; an error here does not
	// invalidate the reindex itself.
	head, _ := gitwatch.Querier{}.HEAD(appRec.RootPath)
	fmt.Fprintf(w, "reindex complete: repo %s at SHA %s\n", appRec.RepoID, head)
	return nil
}

// DefaultDial sends the eng_reindex_repo RPC to the daemon. Either repoID or
// rootPath may be empty; the handler accepts either form. It is the production
// Params.DialReindex.
func DefaultDial(ctx context.Context, repoID, rootPath string) (string, error) {
	type result struct {
		RepoID string `json:"repo_id"`
		Branch string `json:"branch"`
		Status string `json:"status"`
	}
	params := map[string]any{}
	if repoID != "" {
		params["repo_id"] = repoID
	}
	if rootPath != "" {
		params["root_path"] = rootPath
	}
	var r result
	if err := mcpclient.Call(ctx, "eng_reindex_repo", params, &r); err != nil {
		return "", err
	}
	return r.RepoID, nil
}

// MergeTarget combines the positional arg and --repo flag into a single target
// string. The positional form wins when both are supplied; a one-line stderr
// note flags the override so a CI invocation that ends up passing both doesn't
// silently drop one.
func MergeTarget(stderr io.Writer, args []string, repoFlag string) string {
	var positional string
	if len(args) == 1 {
		positional = args[0]
	}
	if positional != "" && repoFlag != "" && positional != repoFlag {
		fmt.Fprintf(stderr, "reindex: positional arg %q overrides --repo %q\n", positional, repoFlag)
		return positional
	}
	if positional != "" {
		return positional
	}
	return repoFlag
}

// resolveTargetForDial converts the user-supplied target into a (repoID,
// rootPath) pair suitable for eng_reindex_repo. When target is empty, the CWD
// is used as the rootPath (daemon canonicalises). A non-empty target is passed
// through as repoID first; the daemon falls back to NotFound rather than the
// CLI guessing, which keeps the resolution rules in one place. A target that
// exists as a directory is sent as rootPath instead so the daemon resolves by
// path.
func resolveTargetForDial(_ context.Context, target string) (repoID, rootPath string, err error) {
	if target == "" {
		cwd, werr := os.Getwd()
		if werr != nil {
			return "", "", fmt.Errorf("reindex: getwd: %w", werr)
		}
		return "", cwd, nil
	}
	if info, serr := os.Stat(target); serr == nil && info.IsDir() {
		abs, aerr := filepath.Abs(target)
		if aerr != nil {
			return "", "", fmt.Errorf("reindex: abs %q: %w", target, aerr)
		}
		return "", abs, nil
	}
	return target, "", nil
}

// resolveReindexTarget picks the repo to reindex. With no target, the cwd is
// canonicalised and matched against every registered repo's RootPath; with a
// target, it is treated first as a repo id (or short_id/alias/prefix) and, on
// miss, as a path (canonicalised + matched against List). matchByPath is the
// cmd-owned cwd→repo seam shared with `veska search`.
func resolveReindexTarget(ctx context.Context, db *sql.DB, target string, matchByPath func(context.Context, *sql.DB, string) (repo.Record, error)) (repo.Record, error) {
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return repo.Record{}, fmt.Errorf("reindex: getwd: %w", err)
		}
		return matchByPath(ctx, db, cwd)
	}

	// Try as a full id, short_id, or unambiguous prefix.
	records, lerr := repo.List(ctx, db)
	if lerr != nil {
		return repo.Record{}, fmt.Errorf("reindex: list repos: %w", lerr)
	}
	if rec, rerr := repocmd.ResolveCLIRepoID(records, target); rerr == nil {
		return rec, nil
	}

	// Then as a filesystem path.
	if _, statErr := os.Stat(target); statErr == nil {
		return matchByPath(ctx, db, target)
	}
	return repo.Record{}, fmt.Errorf("reindex: repo %q is not registered (not a known id and not a registered path)", target)
}
