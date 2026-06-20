// SPDX-License-Identifier: AGPL-3.0-only

// Package wikicmd holds the business logic behind the `veska wiki` command:
// opening the SQLite pools, replicating the daemon's WorkKindWiki handler
// wiring, resolving the target repo/branch, and driving the render.
// cmd/veska/wiki.go is reduced to Cobra command construction whose RunE body
// parses flags/positionals and delegates here (, following the
// cmd = glue / logic-in-packages pattern established by symbolcmd, graphcmd,
// findingscmd, and searchcmd).
package wikicmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// Params bundles the resolved inputs of Run. The positional/flag merge and the
// all mutual-exclusion check stay in the Cobra layer; Run receives a single
// already-resolved RepoID (empty = auto-resolve) plus the All sweep toggle.
type Params struct {
	RepoID string
	Branch string
	All    bool
	Out    io.Writer
	ErrOut io.Writer
}

// Run regenerates the wiki pages (hot_zones + entry_points) by reusing the
// WorkKindWiki render orchestration (wiki.Handler.Handle) - the same code path
// the post-promotion queue lane runs, so the output is byte-identical.
func Run(ctx context.Context, p Params) error {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("wiki: open sqlite pools: %w", err)
	}
	defer func() { _ = pools.Close() }()
	// Apply migrations so the daemon_state table backing the render store
	// exists even on a freshly created database.
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		return fmt.Errorf("wiki: migrate sqlite: %w", err)
	}

	handler, err := buildWikiHandler(pools)
	if err != nil {
		return err
	}

	if p.All {
		return runAll(ctx, pools.ReadDB, handler, p.Out, p.ErrOut)
	}

	resolvedRepo, resolvedBranch, err := ResolveTarget(ctx, pools.ReadDB, p.RepoID, p.Branch)
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

	fmt.Fprintf(p.Out, "wiki regenerated: %s, %s\n", wiki.HotZonesPagePath, wiki.EntryPointsPagePath)
	return nil
}

// runAll renders every registered repo so multi-repo workspaces
// don't have to cd into each repo and re-run. Per-repo failures are logged
// inline but don't abort the sweep - a stuck repo must not suppress the others.
func runAll(ctx context.Context, db *sql.DB, handler *wiki.Handler, out, errOut io.Writer) error {
	records, err := repo.List(ctx, db)
	if err != nil {
		return fmt.Errorf("wiki: list repos: %w", err)
	}
	if len(records) == 0 {
		return fmt.Errorf("wiki: no repos registered - run 'veska repo add <path>' first")
	}
	var failed int
	for _, rec := range records {
		br := rec.ActiveBranch
		if br == "" {
			br = "main"
		}
		row := ports.WorkRow{Kind: ports.WorkKindWiki, RepoID: rec.RepoID, Branch: br}
		if err := handler.Handle(ctx, row); err != nil {
			fmt.Fprintf(errOut, "wiki: %s: %v\n", repocmd.ShortRepoID(rec.RepoID), err)
			failed++
			continue
		}
		fmt.Fprintf(out, "wiki regenerated for %s (%s): %s, %s\n",
			repocmd.ShortRepoID(rec.RepoID), br, wiki.HotZonesPagePath, wiki.EntryPointsPagePath)
	}
	if failed > 0 {
		return fmt.Errorf("wiki: %d of %d repos failed", failed, len(records))
	}
	return nil
}

// ResolveTarget resolves the --repo and --branch flags against the repos
// registry. An empty repoID defaults to the sole registered repo; zero or more
// than one registered repo with an empty flag is an error. An empty branch
// defaults to the resolved repo's active branch.
func ResolveTarget(ctx context.Context, db *sql.DB, repoID, branch string) (string, string, error) {
	records, err := repo.List(ctx, db)
	if err != nil {
		return "", "", fmt.Errorf("wiki: list repos: %w", err)
	}

	var rec repo.Record
	if repoID == "" {
		switch len(records) {
		case 0:
			return "", "", fmt.Errorf("wiki: no repos registered - run 'veska repo add <path>' first")
		case 1:
			rec = records[0]
		default:
			// with multiple repos registered, try the caller's
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
				return "", "", fmt.Errorf("wiki: %d repos registered - pass --repo to choose one, or cd into a registered repo", len(records))
			}
		}
	} else {
		// Match the MCP resolveRepoID progression so the CLI honors the same
		// short_id / prefix contract: exact full id, then
		// ShortRepoIDLen-char short_id, then unambiguous >= 4-char prefix.
		// on id miss, try the same value as a filesystem path
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
	// (one-shot CLI - nothing is staged), the CLI prefix-matching repo
	// resolver, and writePages=true regardless of the daemon's [wiki]
	// write_pages default. The handler graph itself is built by
	// the shared composition constructor.
	wikiRoot := func(ctx context.Context, repoID string) (string, error) {
		records, err := repo.List(ctx, pools.ReadDB)
		if err != nil {
			return "", fmt.Errorf("wiki: repo root lookup: %w", err)
		}
		// ResolveTarget already canonicalized repoID to the full sha, so
		// equality is the expected hit. Keep the prefix resolver as a defensive
		// fallback for any caller that bypasses it.
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
	return composition.NewWikiHandler(pools, staging.NewArea(), wikiRoot, composition.WithWritePages(true))
}
