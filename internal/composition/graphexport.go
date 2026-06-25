// SPDX-License-Identifier: AGPL-3.0-only

package composition

import (
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/graphexport"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// NewGraphExportService wires the in-process graph-export service from a read
// pool. It reuses the same graph, hot-zone, entry-point, and dependency
// primitives the daemon uses, so the snapshot is a faithful projection of the
// live graph and stays consistent with the wiki pages and eng_list_dependencies
// output. The export opens the SQLite graph DB directly (no daemon required):
// in WAL mode the read pool coexists with a running daemon's writers.
func NewGraphExportService(pools *sqlite.Pools, staging *staging.Area) (*graphexport.Service, error) {
	graphRepo := sqlite.NewGraphRepo(pools.ReadDB, pools.Write)
	hotZoneSvc, epSvc, err := newWikiServices(pools, staging)
	if err != nil {
		return nil, err
	}
	depsSvc, err := NewDependenciesService(pools)
	if err != nil {
		return nil, err
	}
	svc, err := graphexport.NewService(
		graphRepo.LoadGraph,
		hotZoneSvc.Rank,
		epSvc.Select,
		depsSvc.List,
		graphRepo.NodeSnippets,
	)
	if err != nil {
		return nil, fmt.Errorf("graph-export service: %w", err)
	}
	return svc, nil
}
