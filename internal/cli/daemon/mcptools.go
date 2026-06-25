// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/resolver"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// mcpToolWiring carries the collaborators shared across the MCP tool families
// so each registration helper stays small and the graph/blast/resolver
// adapters are built once instead of threaded through every call. It mirrors
// the per-call struct pattern used elsewhere (e.g. sqlite.promotion).
type mcpToolWiring struct {
	r     *mcp.Registry
	d     mcpDeps
	pools *sqlite.Pools

	graph          *sqlite.GraphRepo
	edges          *sqlite.EdgeReaderRepo
	nodes          *sqlite.NodeLookupRepo
	findingQuerier *sqlite.FindingQuerierRepo
	blast          *blastradius.Service

	// Cross-repo stub resolvers turn cross_repo_edge_stubs into synthetic
	// ResolvedEdges. resolveStubs is the outbound ("what does this reach")
	// direction; resolveInboundStubs is the reverse ("who calls this",
	// ). Shared by the graph, blast, and context-pack tools.
	resolveStubs        func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error)
	resolveInboundStubs func(ctx context.Context, dstNodeID, branch string) ([]ports.ResolvedEdge, error)
}

// reconcileReader returns the wake reconciler as an mcp.ReconcileReader, or a
// nil interface when no reconciler is wired. Returning a nil interface (rather
// than a typed-nil *WakeReconciler) keeps the helper's nil-check meaningful so
// a query never dereferences a nil reconciler.
func (w *mcpToolWiring) reconcileReader() mcp.ReconcileReader {
	if w.d.reconciler == nil {
		return nil
	}
	return w.d.reconciler
}

// registerMCPTools wires every MCP tool family onto the registry. The call
// order is preserved exactly from the historical monolith so tool registration
// order is unchanged.
func registerMCPTools(r *mcp.Registry, d mcpDeps) error {
	w, err := newMCPToolWiring(r, d)
	if err != nil {
		return err
	}
	w.registerBasicDataTools()
	w.registerPromotionTools()
	w.registerOwnerTodoAdminTools()
	w.registerGraphTools()
	w.registerChangedSymbolsTool()
	w.registerWikiTools()
	w.registerContextPackTool()
	if err := w.registerSearchTool(); err != nil {
		return err
	}
	w.registerDependenciesTool()
	w.registerCloneTool()
	return nil
}

func newMCPToolWiring(r *mcp.Registry, d mcpDeps) (*mcpToolWiring, error) {
	pools := d.pools
	w := &mcpToolWiring{
		r:              r,
		d:              d,
		pools:          pools,
		graph:          sqlite.NewGraphRepo(pools.ReadDB, pools.Write),
		edges:          sqlite.NewEdgeReaderRepo(pools.ReadDB),
		nodes:          sqlite.NewNodeLookupRepo(pools.ReadDB),
		findingQuerier: sqlite.NewFindingQuerierRepo(pools.ReadDB),
	}
	// A zero hubDegreeThreshold means an unconfigured (legacy/test) mcpDeps;
	// leave the blast service on its seeded DefaultHubDegreeThreshold rather
	// than overwriting it with 0. config.Validate rejects a 0 in config.toml,
	// so a live daemon always passes a non-zero value here.
	var blastOpts []blastradius.ServiceOption
	if d.hubDegreeThreshold != 0 {
		blastOpts = append(blastOpts, blastradius.WithDefaultHubDegreeThreshold(d.hubDegreeThreshold))
	}
	blast, err := blastradius.NewService(w.edges, w.nodes, d.staging, blastOpts...)
	if err != nil {
		return nil, fmt.Errorf("mcp tools: blast-radius service: %w", err)
	}
	w.blast = blast
	w.resolveStubs = func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error) {
		return resolver.ResolveStubsForNode(ctx, pools.ReadDB, nodeID, branch, expand)
	}
	w.resolveInboundStubs = func(ctx context.Context, dstNodeID, branch string) ([]ports.ResolvedEdge, error) {
		return resolver.ResolveStubsTargetingNode(ctx, pools.ReadDB, dstNodeID, branch)
	}
	return w, nil
}

// repos returns a fresh repo lister over the read pool. Construction is cheap
// (a struct over the shared *sql.DB) so each tool gets its own as before.
func (w *mcpToolWiring) repos() *repoLister { return &repoLister{db: w.pools.ReadDB} }

// registerBasicDataTools registers the tools needing only *sql.DB + AuditWriter.
func (w *mcpToolWiring) registerBasicDataTools() {
	pools := w.pools
	mcp.RegisterFindingTools(w.r, pools.Write, nil, w.repos())
	mcp.RegisterSuppressionTools(w.r, pools.Write, nil, w.repos())
	mcp.RegisterRecordTools(w.r, pools.Write, nil)
	reg := w.d.regSvc
	if reg == nil {
		reg = &repoRegistrar{db: pools.Write}
	}
	mcp.RegisterRepoTools(w.r, reg, w.repos())
}

// registerPromotionTools registers eng_promote and eng_reindex_repo
// Each degrades cleanly (skipped) when its deps are nil - legacy
// or test wiring - rather than panicking at startup.
func (w *mcpToolWiring) registerPromotionTools() {
	if w.d.ingester != nil && w.d.promoter != nil {
		mcp.RegisterPromoteTool(w.r, mcp.PromoteDeps{
			Repos:    w.repos(),
			Git:      gitwatch.Querier{},
			Ingester: w.d.ingester,
			Promoter: w.d.promoter,
		})
	}
	if w.d.reparser != nil {
		mcp.RegisterReindexTool(w.r, mcp.ReindexDeps{
			Repos:    w.repos(),
			Reparser: w.d.reparser,
		})
	}
}

// registerOwnerTodoAdminTools registers owner, todo, and admin tools. Task
// tools are PARKED: there is no MCP path to create a task, so
// exposing them surfaces dead-end UX. The keep-alive reference re-enables a
// clean re-registration when a task backend lands.
func (w *mcpToolWiring) registerOwnerTodoAdminTools() {
	_ = mcp.RegisterTaskTools // keep the symbol reachable for the future re-enable
	mcp.RegisterOwnerTools(w.r, w.pools.Write, w.repos())
	mcp.RegisterTodoTools(w.r, sqlite.NewTodoQuerierRepo(w.pools.ReadDB), w.repos())
	mcp.RegisterAdminTools(w.r,
		w.repos(),
		&statusProvider{db: w.pools.ReadDB, scans: w.d.scanTracker, pressure: w.d.memPressure},
		&configProvider{cfg: w.d.cfg},
	)
}

// registerGraphTools registers the graph + blast-radius tools. The cross-repo
// resolvers turn cross_repo_edge_stubs into synthetic ResolvedEdges for
// call_chain and blast_radius; without them the xc51.3 stub
// producer has no consumer.
func (w *mcpToolWiring) registerGraphTools() {
	mcp.RegisterGraphTools(w.r, w.graph, w.d.staging,
		mcp.WithRepoLister(w.repos()),
		mcp.WithResolveFunc(w.resolveStubs),
		mcp.WithInboundResolveFunc(w.resolveInboundStubs),
		mcp.WithScanTracker(w.d.scanTracker),
		mcp.WithReconcileTracker(w.reconcileReader()),
	)
	mcp.RegisterBlastTools(w.r, w.blast, repoRootFunc(w.pools.ReadDB), gitwatch.ChangedFiles, w.repos(), w.graph,
		mcp.WithBlastChangedFilesBetween(gitwatch.ChangedFilesBetween),
		mcp.WithBlastResolveFunc(w.resolveStubs),
		mcp.WithBlastInboundResolveFunc(w.resolveInboundStubs),
		mcp.WithBlastScanTracker(w.d.scanTracker),
		mcp.WithBlastReconcileTracker(w.reconcileReader()))
}

// registerChangedSymbolsTool registers eng_find_changed_symbols, which parses
// each file changed between two git refs at both refs and diffs the symbol
// sets - no promoted-graph history substrate needed. fileAtRef wraps the git
// adapter's ErrFileNotAtRef so the service distinguishes "file absent at ref"
// from "ref tree unreadable".
func (w *mcpToolWiring) registerChangedSymbolsTool() {
	fileAtRef := func(ctx context.Context, root, ref, path string) ([]byte, error) {
		b, err := gitwatch.FileAtRef(ctx, root, ref, path)
		if err != nil && errors.Is(err, gitwatch.ErrFileNotAtRef) {
			return nil, fmt.Errorf("%w: %v", changedsymbols.ErrFileAbsentAtRef, err)
		}
		return b, err
	}
	csSvc, err := changedsymbols.NewService(
		treesitter.NewGoParser(), gitwatch.ChangedFilesBetween, fileAtRef,
	)
	if err != nil {
		mcp.RegisterChangedSymbolsTool(w.r, nil, nil, w.repos())
		return
	}
	mcp.RegisterChangedSymbolsTool(w.r, csSvc, repoRootFunc(w.pools.ReadDB), w.repos())
}

// registerWikiTools registers the hot_zone and entry_points surfaces. Change
// frequency comes from git commit history; the entry-point safety gates draw on
// edge adjacency, blast radius, and the findings table.
func (w *mcpToolWiring) registerWikiTools() {
	hotZoneCounts := func(ctx context.Context, repoRoot string) (map[string]int, error) {
		return gitwatch.ChangeCounts(ctx, repoRoot, 0)
	}
	if hotZoneSvc, err := wiki.NewHotZoneService(hotZoneCounts, w.nodes.NodesInFile, w.blast); err == nil {
		mcp.RegisterWikiTools(w.r, hotZoneSvc, repoRootFunc(w.pools.ReadDB), w.repos())
	} else {
		mcp.RegisterWikiTools(w.r, nil, nil, w.repos())
	}

	if epSvc, err := wiki.NewEntryPointsService(
		w.graph.LoadGraph, w.edges.InboundEdges, w.findingQuerier.OpenFindingNodeIDs,
	); err == nil {
		mcp.RegisterEntryPointsTool(w.r, epSvc, w.repos())
	} else {
		mcp.RegisterEntryPointsTool(w.r, nil, w.repos())
	}
}

// registerContextPackTool registers eng_get_context_pack, which assembles a
// token-bounded bundle of relevant nodes / commits / findings / tasks for a
// symbol or task. Commits come from the git history reader; the active task
// from the tasks table.
func (w *mcpToolWiring) registerContextPackTool() {
	fileHistory := func(ctx context.Context, repoRoot, path string, window time.Duration) ([]contextpack.CommitInfo, error) {
		commits, err := gitwatch.FileHistory(ctx, repoRoot, path, window)
		if err != nil {
			return nil, err
		}
		out := make([]contextpack.CommitInfo, 0, len(commits))
		for _, c := range commits {
			out = append(out, contextpack.CommitInfo{
				Hash: c.Hash, Author: c.Author, When: c.When, Subject: c.Subject,
			})
		}
		return out, nil
	}
	cpAsm, err := contextpack.NewAssembler(contextpack.AssemblerDeps{
		FindNodes:    w.graph.FindNodes,
		Blast:        w.blast,
		FileHistory:  fileHistory,
		OpenFindings: w.findingQuerier.OpenFindingNodeIDs,
		ChangedFiles: gitwatch.ChangedFiles,
		NodesInFile:  w.nodes.NodesInFile,
		ActiveTask:   sqlite.NewTaskRepo(w.pools.ReadDB).GetActiveTask,
	})
	if err != nil {
		mcp.RegisterContextPackTool(w.r, nil, nil, w.repos())
		return
	}
	mcp.RegisterContextPackTool(w.r, cpAsm, repoRootFunc(w.pools.ReadDB), w.repos(),
		mcp.WithContextPackResolveFunc(w.resolveStubs),
		mcp.WithContextPackInboundResolveFunc(w.resolveInboundStubs),
		mcp.WithContextPackScanTracker(w.d.scanTracker),
		mcp.WithContextPackReconcileTracker(w.reconcileReader()))
}

// registerSearchTool registers the semantic-search tools. The Service
// orchestrates embed → vector search → node hydration with lexical fallback.
func (w *mcpToolWiring) registerSearchTool() error {
	searchSvc, err := search.NewService(w.d.provider, w.d.vectors, w.nodes,
		search.WithMetrics(w.d.metrics))
	if err != nil {
		return fmt.Errorf("mcp tools: search service: %w", err)
	}
	mcp.RegisterSearchTools(w.r, searchSvc, w.d.refs, w.d.vectors, w.nodes, w.d.savings, w.repos(),
		mcp.WithSearchGraph(w.graph),
		mcp.WithSearchScanTracker(w.d.scanTracker),
		mcp.WithSearchReconcileTracker(w.reconcileReader()),
		mcp.WithSearchFTSPending(sqlite.NewFTSPendingCounter(w.pools.ReadDB)))
	return nil
}

// registerCloneTool registers eng_find_clones: exact-clone detection
// by content_hash equality (mode=exact) plus near-duplicate clustering over
// thresholded SIMILAR_TO edges (mode=near). One CloneRepo
// satisfies both the CloneStore and NearStore ports. If construction fails
// (only a nil store, which cannot happen here) the tool is skipped rather than
// aborting daemon startup.
func (w *mcpToolWiring) registerCloneTool() {
	repo := sqlite.NewCloneRepo(w.pools.ReadDB)
	// The elected embedder's ModelID selects the calibrated near-dup default
	// score spaces differ per model, so the threshold must too.
	var embedderID string
	if w.d.provider != nil {
		embedderID = w.d.provider.ModelID()
	}
	finder, err := duplicates.NewFinder(repo, repo, embedderID)
	if err != nil {
		return
	}
	mcp.RegisterCloneTools(w.r, finder, w.repos())
}

// registerDependenciesTool registers eng_list_dependencies, which
// aggregates per-repo cross-repo edge stubs into a ranked module list with
// sample call sites and go.mod versions.
func (w *mcpToolWiring) registerDependenciesTool() {
	depsSvc, err := composition.NewDependenciesService(w.pools)
	if err != nil {
		mcp.RegisterDependenciesTool(w.r, nil, w.repos())
		return
	}
	mcp.RegisterDependenciesTool(w.r, depsSvc, w.repos())
}
