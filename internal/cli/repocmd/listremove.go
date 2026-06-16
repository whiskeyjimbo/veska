package repocmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/extindex"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// RunRepoList prints every registered repo. Prefers the running
// daemon's eng_list_repos so the listing matches what the daemon sees
// (including in-flight scan state); falls back to a direct SQLite read so the
// CLI still works when the daemon is down.
func RunRepoList(ctx context.Context, w io.Writer, includeExternal bool) error {
	var lr listResult
	params := map[string]any{}
	if includeExternal {
		params["include_vendored"] = true
	}
	if err := mcpclient.Call(ctx, "eng_list_repos", params, &lr); err == nil {
		PrintRepoTableWithProgress(w, lr.Repos, FetchScanProgress(ctx))
		return nil
	}

	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return fmt.Errorf("repo list: %w", err)
	}
	defer closeFn()
	recs, err := repo.List(ctx, db)
	if err != nil {
		return fmt.Errorf("repo list: %w", err)
	}
	views := make([]RepoView, 0, len(recs))
	for _, r := range recs {
		if !includeExternal && strings.HasPrefix(r.RepoID, extindex.SyntheticRepoIDPrefix) {
			continue
		}
		views = append(views, recordToView(r))
	}
	PrintRepoTable(w, views)
	return nil
}

// recordToView projects a repo.Record onto the MCP-shaped RepoView.
func recordToView(r repo.Record) RepoView {
	return RepoView{
		RepoID:          r.RepoID,
		RootPath:        r.RootPath,
		ActiveBranch:    r.ActiveBranch,
		LastPromotedSHA: r.LastPromotedSHA,
		Kind:            r.Kind,
		Aliases:         r.Aliases,
	}
}

// RunRepoRemoveOne is the original single-repo deregister path.
func RunRepoRemoveOne(ctx context.Context, w io.Writer, arg string) error {
	// accept the same identifiers `repo add` does.
	id, resolveErr := ResolveRepoArg(ctx, arg)
	if resolveErr != nil {
		return fmt.Errorf("repo remove: %w", resolveErr)
	}
	if err := dialRemoveRepo(ctx, id); err == nil {
		fmt.Fprintln(w, "removed (via daemon)")
		return nil
	}
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return fmt.Errorf("repo remove: %w", err)
	}
	defer closeFn()
	if err := repo.Remove(ctx, db, id); err != nil {
		return fmt.Errorf("repo remove: %w", err)
	}
	fmt.Fprintln(w, "removed (direct write; daemon offline)")
	return nil
}

// listRegisteredRepos returns the repo set via the daemon when up, falling
// back to direct DB read. Shared by RunRepoRemoveMissing / RunRepoRemoveAll so
// the unified verb sees the same registry the legacy prune did.
// pass include_vendored=true so synthetic ext:<module> rows
// created by `veska deps index` are surfaced; without this they stayed
// invisible to `repo remove --missing` and orphaned on vendor-dir deletion.
func listRegisteredRepos(ctx context.Context) ([]RepoView, error) {
	var lr listResult
	if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{"include_vendored": true}, &lr); err == nil {
		return lr.Repos, nil
	}
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return nil, err
	}
	defer closeFn()
	recs, err := repo.List(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]RepoView, 0, len(recs))
	for _, r := range recs {
		out = append(out, recordToView(r))
	}
	return out, nil
}

// RunRepoRemoveMissing is the old `repo prune` body, lifted under the unified
// verb. Daemon-up uses dialRemoveRepo; daemon-down uses direct repo.Remove.
func RunRepoRemoveMissing(ctx context.Context, w io.Writer, dryRun bool) error {
	repos, err := listRegisteredRepos(ctx)
	if err != nil {
		return fmt.Errorf("repo remove --missing: %w", err)
	}
	var missing []RepoView
	for _, r := range repos {
		if _, statErr := os.Stat(r.RootPath); errors.Is(statErr, os.ErrNotExist) {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintln(w, "no missing repos — nothing to remove")
		return nil
	}
	return applyBulkRemove(ctx, w, missing, dryRun, "missing")
}

// RunRepoRemoveAll wipes the whole registry. Requires --yes when non-interactive.
func RunRepoRemoveAll(ctx context.Context, w io.Writer, in io.Reader, dryRun, yes bool) error {
	repos, err := listRegisteredRepos(ctx)
	if err != nil {
		return fmt.Errorf("repo remove --all: %w", err)
	}
	if len(repos) == 0 {
		fmt.Fprintln(w, "registry is empty — nothing to remove")
		return nil
	}
	if !dryRun && !yes {
		fmt.Fprintf(w, "about to remove all %d registered repo(s). Continue? [y/N] ", len(repos))
		var resp string
		_, _ = fmt.Fscanln(in, &resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Fprintln(w, "aborted")
			return nil
		}
	}
	return applyBulkRemove(ctx, w, repos, dryRun, "all")
}

// applyBulkRemove iterates targets and removes each via daemon-or-direct,
// printing per-row status and a trailing summary. Errors on individual rows
// are printed but do not abort the loop — partial cleanup is better than none.
func applyBulkRemove(ctx context.Context, w io.Writer, targets []RepoView, dryRun bool, scope string) error {
	useDaemon := false
	if _, err := dialEngStatus(ctx); err == nil {
		useDaemon = true
	}
	var db *sql.DB
	if !useDaemon {
		var closeFn func()
		var err error
		db, closeFn, err = OpenLocalDB()
		if err != nil {
			return fmt.Errorf("repo remove --%s: %w", scope, err)
		}
		defer closeFn()
	}
	for _, r := range targets {
		removeBulkRow(ctx, w, db, r, bulkRemoveOpts{UseDaemon: useDaemon, DryRun: dryRun})
	}
	if dryRun {
		fmt.Fprintf(w, "%d candidate(s) — rerun without --dry-run to apply\n", len(targets))
	} else {
		fmt.Fprintf(w, "removed %d repo(s)\n", len(targets))
	}
	return nil
}

// bulkRemoveOpts carries the per-row removal mode: daemon-vs-direct and
// whether this is a dry run.
type bulkRemoveOpts struct {
	UseDaemon bool
	DryRun    bool
}

// removeBulkRow prints and applies the removal of a single bulk target.
func removeBulkRow(ctx context.Context, w io.Writer, db *sql.DB, r RepoView, opts bulkRemoveOpts) {
	prefix := "would remove"
	if !opts.DryRun {
		prefix = "removing"
	}
	fmt.Fprintf(w, "%s %s  %s\n", prefix, ShortRepoID(r.RepoID), r.RootPath)
	if opts.DryRun {
		return
	}
	var rmErr error
	if opts.UseDaemon {
		rmErr = dialRemoveRepo(ctx, r.RepoID)
	} else {
		rmErr = repo.Remove(ctx, db, r.RepoID)
	}
	if rmErr != nil {
		fmt.Fprintf(w, "  failed: %v\n", rmErr)
	}
}

// RunRepoAlias binds a human-friendly name to a repo. Resolves
// target against the standard progression (full id, short_id, alias, prefix);
// force overwrites an existing binding. Surfaces a swap hint when the args
// look reversed.
func RunRepoAlias(ctx context.Context, w io.Writer, name, target string, force bool) error {
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return fmt.Errorf("repo alias: %w", err)
	}
	defer closeFn()

	recs, err := repo.List(ctx, db)
	if err != nil {
		return fmt.Errorf("repo alias: %w", err)
	}
	rec, err := ResolveCLIRepoID(recs, target)
	if err != nil {
		// Likely arg-order mistake: `veska repo alias <id> <name>` instead of
		// the documented `<name> <id>`. If args[0] resolves and args[1] does
		// not, surface the swap hint instead of the generic error.
		if _, swapErr := ResolveCLIRepoID(recs, name); swapErr == nil {
			return fmt.Errorf("repo alias: %w — did you swap the arguments? usage: `veska repo alias <name> <repo-id-or-prefix-or-alias>` (got name=%q repo=%q)", err, name, target)
		}
		return fmt.Errorf("repo alias: %w", err)
	}
	if err := repo.SetAlias(ctx, db, name, rec.RepoID, force); err != nil {
		if errors.Is(err, repo.ErrAliasExists) {
			return fmt.Errorf("repo alias: %w (re-run with --force to overwrite)", err)
		}
		return fmt.Errorf("repo alias: %w", err)
	}
	fmt.Fprintf(w, "aliased %q to %s\n", name, ShortRepoID(rec.RepoID))
	return nil
}

// RunRepoUnalias removes a user-defined alias. Errors on unknown
// name so a typo doesn't silently succeed.
func RunRepoUnalias(ctx context.Context, w io.Writer, name string) error {
	db, closeFn, err := OpenLocalDB()
	if err != nil {
		return fmt.Errorf("repo unalias: %w", err)
	}
	defer closeFn()
	if err := repo.RemoveAlias(ctx, db, name); err != nil {
		return fmt.Errorf("repo unalias: %w", err)
	}
	fmt.Fprintf(w, "removed alias %q\n", name)
	return nil
}
