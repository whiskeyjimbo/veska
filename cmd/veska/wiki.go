package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// wikiCmd returns the "wiki" Cobra command. It regenerates both wiki pages
// (hot_zones + entry_points) on demand by reusing the WorkKindWiki render
// orchestration (wiki.Handler.Handle) — the same code path the post-promotion
// queue lane runs, so the output is byte-identical.
func wikiCmd() *cobra.Command {
	var repoID, branch string

	cmd := &cobra.Command{
		Use:          "wiki",
		Short:        "Regenerate the veska wiki pages (hot_zones + entry_points)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
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
			resolvedRepo, resolvedBranch, err := resolveWikiTarget(ctx, pools.ReadDB, repoID, branch)
			if err != nil {
				return err
			}

			handler, err := buildWikiHandler(pools)
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
			return "", "", fmt.Errorf("wiki: %d repos registered — pass --repo to choose one", len(records))
		}
	} else {
		found := false
		for _, r := range records {
			if r.RepoID == repoID {
				rec, found = r, true
				break
			}
		}
		if !found {
			return "", "", fmt.Errorf("wiki: repo %q is not registered", repoID)
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
	staging := application.NewStagingArea()

	nodeLookup := sqlite.NewNodeLookupRepo(pools.ReadDB)
	wikiEdges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	wikiGraph := sqlite.NewGraphRepo(pools.ReadDB, pools.WriteHot)
	wikiFindings := sqlite.NewFindingQuerierRepo(pools.ReadDB)
	wikiBlast := blastradius.NewService(wikiEdges, nodeLookup, staging)

	wikiCounts := func(ctx context.Context, repoRoot string) (map[string]int, error) {
		return gitwatch.ChangeCounts(ctx, repoRoot, 0)
	}
	hotZoneSvc, err := wiki.NewHotZoneService(wikiCounts, nodeLookup.NodesInFile, wikiBlast)
	if err != nil {
		return nil, fmt.Errorf("wiki: hot-zone service: %w", err)
	}
	epSvc, err := wiki.NewEntryPointsService(
		wikiGraph.LoadGraph, wikiEdges.InboundEdges, wikiFindings.OpenFindingNodeIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("wiki: entry-points service: %w", err)
	}

	wikiRoot := func(ctx context.Context, repoID string) (string, error) {
		records, err := repo.List(ctx, pools.ReadDB)
		if err != nil {
			return "", fmt.Errorf("wiki: repo root lookup: %w", err)
		}
		for _, rec := range records {
			if rec.RepoID == repoID {
				return rec.RootPath, nil
			}
		}
		return "", fmt.Errorf("wiki: repo %q is not registered", repoID)
	}

	handler, err := wiki.NewHandler(
		hotZoneSvc, epSvc,
		sqlite.NewWikiRenderStateRepo(pools.ReadDB, pools.WriteHot),
		wikiRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("wiki: handler: %w", err)
	}
	return handler, nil
}
