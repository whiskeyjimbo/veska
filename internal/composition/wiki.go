package composition

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// wikiHandlerConfig holds the tuneable knobs for NewWikiHandler.
type wikiHandlerConfig struct {
	writePages bool
}

// WikiHandlerOption configures NewWikiHandler.
type WikiHandlerOption func(*wikiHandlerConfig)

// WithWritePages enables writing docs/veska/*.md into the repo working tree on
// every promotion. Absent ⇒ false (no files written).
func WithWritePages(write bool) WikiHandlerOption {
	return func(c *wikiHandlerConfig) { c.writePages = write }
}

// NewWikiHandler builds the WorkKindWiki regeneration handler (the hot_zone and
// entry_points surfaces). It is shared by the daemon's queue lane and the CLI
// `veska wiki` command, which previously held byte-for-byte copies kept in sync
// by hand (solov2-u4mv.4).
//
// The three caller differences are parameters:
//   - staging: the daemon shares its live StagingArea so blast radius sees
//     in-flight (staged-but-unpromoted) nodes; the CLI passes a fresh one.
//   - repoRoot: the daemon resolves repoID→root via the repos table; the CLI
//     uses its prefix-matching resolver.
//   - write-pages: `veska wiki` always writes pages (WithWritePages(true)); the
//     daemon honours the [wiki] write_pages config.
//
// Write-pages defaults to false (no files written) when WithWritePages is
// absent, matching the README's "no files written to disk" contract.
func NewWikiHandler(pools *sqlite.Pools, staging *staging.Area, repoRoot func(ctx context.Context, repoID string) (string, error), opts ...WikiHandlerOption) (*wiki.Handler, error) {
	var cfg wikiHandlerConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	nodeLookup := sqlite.NewNodeLookupRepo(pools.ReadDB)
	wikiEdges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	wikiGraph := sqlite.NewGraphRepo(pools.ReadDB, pools.Write)
	wikiFindings := sqlite.NewFindingQuerierRepo(pools.ReadDB)
	wikiBlast, err := blastradius.NewService(wikiEdges, nodeLookup, staging)
	if err != nil {
		return nil, fmt.Errorf("wiki: blast-radius service: %w", err)
	}

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
	handler, err := wiki.NewHandler(
		hotZoneSvc, epSvc,
		sqlite.NewWikiRenderStateRepo(pools.ReadDB, pools.Write),
		repoRoot,
		wiki.WithWritePages(cfg.writePages),
	)
	if err != nil {
		return nil, fmt.Errorf("wiki: handler: %w", err)
	}
	return handler, nil
}
