package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// wikiCmd returns the "wiki" Cobra command. It regenerates both wiki pages
// (hot_zones + entry_points) on demand by reusing the WorkKindWiki render
// orchestration (wiki.Handler.Handle) — the same code path the post-promotion
// queue lane runs, so the output is byte-identical.
func wikiCmd() *cobra.Command {
	var (
		repoID  string
		branch  string
		allFlag bool
	)

	cmd := &cobra.Command{
		Use:          "wiki [path|repo-id]",
		Short:        "Regenerate the veska wiki pages (hot_zones + entry_points)",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if allFlag && (repoID != "" || branch != "" || len(args) > 0) {
				return fmt.Errorf("wiki: --all is mutually exclusive with --repo/--branch and positional args")
			}
			// solov2-rtql: accept an optional positional path or repo id so
			// 'veska wiki /path/to/repo' works the same way 'veska reindex'
			// and 'veska repo add' do. The positional arg and --repo flag
			// are mutually exclusive — pick one source of truth.
			if len(args) == 1 {
				if repoID != "" {
					return fmt.Errorf("wiki: pass either a positional repo selector or --repo, not both")
				}
				repoID = args[0]
			}
			w := cmd.OutOrStdout()
			dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")

			pools, err := sqlite.OpenPools(dbPath)
			if err != nil {
				return fmt.Errorf("wiki: open sqlite pools: %w", err)
			}
			defer func() { _ = pools.Close() }()
			// Apply migrations so the daemon_state table backing the render
			// store exists even on a freshly created database.
			if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
				return fmt.Errorf("wiki: migrate sqlite: %w", err)
			}

			ctx := cmd.Context()
			handler, err := buildWikiHandler(pools)
			if err != nil {
				return err
			}

			// solov2-drd2: --all renders every registered repo so users with
			// multi-repo workspaces (the cobra-CLI-plus-shared-lib pattern)
			// don't have to cd into each repo and re-run. Per-repo failures
			// are logged inline but don't abort the sweep — a stuck repo
			// must not suppress the others.
			if allFlag {
				records, err := repo.List(ctx, pools.ReadDB)
				if err != nil {
					return fmt.Errorf("wiki: list repos: %w", err)
				}
				if len(records) == 0 {
					return fmt.Errorf("wiki: no repos registered — run 'veska repo add <path>' first")
				}
				var failed int
				for _, rec := range records {
					br := rec.ActiveBranch
					if br == "" {
						br = "main"
					}
					row := ports.WorkRow{Kind: ports.WorkKindWiki, RepoID: rec.RepoID, Branch: br}
					if err := handler.Handle(ctx, row); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "wiki: %s: %v\n", repocmd.ShortRepoID(rec.RepoID), err)
						failed++
						continue
					}
					fmt.Fprintf(w, "wiki regenerated for %s (%s): %s, %s\n",
						repocmd.ShortRepoID(rec.RepoID), br, wiki.HotZonesPagePath, wiki.EntryPointsPagePath)
				}
				if failed > 0 {
					return fmt.Errorf("wiki: %d of %d repos failed", failed, len(records))
				}
				return nil
			}

			resolvedRepo, resolvedBranch, err := resolveWikiTarget(ctx, pools.ReadDB, repoID, branch)
			if err != nil {
				return err
			}

			row := ports.WorkRow{
				Kind:   ports.WorkKindWiki,
				RepoID: resolvedRepo,
				Branch: resolvedBranch,
			}
			if err := handler.Handle(ctx, row); err != nil {
				return fmt.Errorf("wiki: regenerate: %w", err)
			}

			fmt.Fprintf(w, "wiki regenerated: %s, %s\n", wiki.HotZonesPagePath, wiki.EntryPointsPagePath)
			return nil
		},
	}

	cmd.Flags().StringVar(&repoID, "repo", "", "repo ID to regenerate (default: the sole registered repo)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to regenerate (default: the repo's active branch)")
	cmd.Flags().BoolVar(&allFlag, "all", false, "regenerate the wiki for every registered repo")
	return cmd
}

// resolveWikiTarget resolves the --repo and --branch flags against the repos
// registry. An empty --repo defaults to the sole registered repo; zero or more
// than one registered repo with an empty flag is an error. An empty --branch
// defaults to the resolved repo's active branch.
func resolveWikiTarget(ctx context.Context, db *sql.DB, repoID, branch string) (string, string, error) {
	records, err := repo.List(ctx, db)
	if err != nil {
		return "", "", fmt.Errorf("wiki: list repos: %w", err)
	}

	var rec repo.Record
	if repoID == "" {
		switch len(records) {
		case 0:
			return "", "", fmt.Errorf("wiki: no repos registered — run 'veska repo add <path>' first")
		case 1:
			rec = records[0]
		default:
			// solov2-ig2x: with multiple repos registered, try the caller's
			// cwd before erroring out. Matches what `veska search` does and
			// what the MCP resolveRepoIDOrCwd helper does for query tools.
			if cwd, err := os.Getwd(); err == nil && cwd != "" {
				for _, r := range records {
					if r.RootPath != "" && (cwd == r.RootPath || strings.HasPrefix(cwd, r.RootPath+"/")) {
						rec = r
						break
					}
				}
			}
			if rec.RepoID == "" {
				return "", "", fmt.Errorf("wiki: %d repos registered — pass --repo to choose one, or cd into a registered repo", len(records))
			}
		}
	} else {
		// Match the MCP resolveRepoID progression so the CLI honours the same
		// short_id / prefix contract (solov2-c7lq): exact full id, then
		// ShortRepoIDLen-char short_id, then unambiguous >= 4-char prefix.
		// solov2-rtql: on id miss, try the same value as a filesystem path
		// against every registered repo's RootPath so the positional arg can
		// be either an id or a path (matching `veska reindex`).
		if matched, rerr := repocmd.ResolveCLIRepoID(records, repoID); rerr == nil {
			rec = matched
		} else if _, statErr := os.Stat(repoID); statErr == nil {
			canonical, aerr := filepath.Abs(repoID)
			if aerr != nil {
				return "", "", fmt.Errorf("wiki: abs %q: %w", repoID, aerr)
			}
			if resolved, serr := filepath.EvalSymlinks(canonical); serr == nil {
				canonical = resolved
			}
			for _, r := range records {
				if r.RootPath == canonical {
					rec = r
					break
				}
			}
			if rec.RepoID == "" {
				return "", "", fmt.Errorf("wiki: path %q is not a registered repository", canonical)
			}
		} else {
			return "", "", fmt.Errorf("wiki: %w", rerr)
		}
	}

	if branch == "" {
		branch = rec.ActiveBranch
	}
	return rec.RepoID, branch, nil
}

// buildWikiHandler replicates the daemon's WorkKindWiki wiring (see
// cmd/veska-daemon/wire.go) so the CLI produces byte-identical pages without
// reimplementing render orchestration.
func buildWikiHandler(pools *sqlite.Pools) (*wiki.Handler, error) {
	// `veska wiki` is the explicit, user-invoked render path: a fresh staging
	// (one-shot CLI — nothing is staged), the CLI prefix-matching repo
	// resolver, and writePages=true regardless of the daemon's [wiki]
	// write_pages default (solov2-ocnn). The handler graph itself is built by
	// the shared composition constructor (solov2-u4mv.4).
	wikiRoot := func(ctx context.Context, repoID string) (string, error) {
		records, err := repo.List(ctx, pools.ReadDB)
		if err != nil {
			return "", fmt.Errorf("wiki: repo root lookup: %w", err)
		}
		// resolveWikiTarget already canonicalised repoID to the full sha, so
		// equality is the expected hit. Keep the prefix resolver as a defensive
		// fallback for any caller that bypasses it (solov2-c7lq).
		for _, rec := range records {
			if rec.RepoID == repoID {
				return rec.RootPath, nil
			}
		}
		matched, rerr := repocmd.ResolveCLIRepoID(records, repoID)
		if rerr != nil {
			return "", fmt.Errorf("wiki: %w", rerr)
		}
		return matched.RootPath, nil
	}
	return composition.NewWikiHandler(pools, application.NewStagingArea(), wikiRoot, true)
}
