// SPDX-License-Identifier: AGPL-3.0-only

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

// WithWritePages enables writing wiki pages into the repository working tree on
// every promotion.
func WithWritePages(write bool) WikiHandlerOption {
	return func(c *wikiHandlerConfig) { c.writePages = write }
}

// NewWikiHandler builds the WorkKindWiki regeneration handler. It is shared by
// the daemon queue lane and the CLI wiki command. The staging area, repository
// root resolver, and page writing behavior are configured as parameters to
// allow sharing logic while accommodating caller differences.
func NewWikiHandler(pools *sqlite.Pools, staging *staging.Area, repoRoot func(ctx context.Context, repoID string) (string, error), opts ...WikiHandlerOption) (*wiki.Handler, error) {
	var cfg wikiHandlerConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	hotZoneSvc, epSvc, err := newWikiServices(pools, staging)
	if err != nil {
		return nil, err
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

// newWikiServices builds the hot-zone and entry-point ranking services from a
// read pool + staging area. Shared by NewWikiHandler (queue lane / CLI wiki
// render) and NewGraphExportService (the snapshot reuses the same ranked hot
// zones and entry points), so both surfaces rank through identical wiring.
func newWikiServices(pools *sqlite.Pools, staging *staging.Area) (*wiki.HotZoneService, *wiki.EntryPointsService, error) {
	nodeLookup := sqlite.NewNodeLookupRepo(pools.ReadDB)
	wikiEdges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	wikiGraph := sqlite.NewGraphRepo(pools.ReadDB, pools.Write)
	wikiFindings := sqlite.NewFindingQuerierRepo(pools.ReadDB)
	wikiBlast, err := blastradius.NewService(wikiEdges, nodeLookup, staging)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: blast-radius service: %w", err)
	}

	wikiCounts := func(ctx context.Context, repoRoot string) (map[string]int, error) {
		return gitwatch.ChangeCounts(ctx, repoRoot, 0)
	}
	hotZoneSvc, err := wiki.NewHotZoneService(wikiCounts, nodeLookup.NodesInFile, wikiBlast)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: hot-zone service: %w", err)
	}
	epSvc, err := wiki.NewEntryPointsService(
		wikiGraph.LoadGraph, wikiEdges.InboundEdges, wikiFindings.OpenFindingNodeIDs,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: entry-points service: %w", err)
	}
	return hotZoneSvc, epSvc, nil
}
