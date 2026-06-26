// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"sort"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// CrossRepoEdge represents a synthetic edge that crosses repository boundaries.
type CrossRepoEdge struct {
	SrcNodeID string `json:"src_node_id"`
	DstNodeID string `json:"dst_node_id"`
	DstRepoID string `json:"dst_repo_id"`
	DstBranch string `json:"dst_branch"`
	Kind      string `json:"kind"`
	CrossRepo bool   `json:"cross_repo"` // always true
	// SrcLine is the 1-indexed line of the call expression in the source node's file.
	SrcLine int `json:"src_line,omitempty"`
}

// GraphResponse is the envelope returned by graph queries, defaulting Nodes to a non-nil slice for serialization.
type GraphResponse struct {
	Nodes           []nodeDTO `json:"nodes"`
	IncludedStaging bool      `json:"included_staging"`
	DegradedReasons []string  `json:"degraded_reasons"`
	// IndexingRepos lists repositories with cold scans in flight at query time.
	IndexingRepos []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos lists repositories undergoing wake reconciliation at query time.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// callChainResponse is the envelope returned by eng_get_call_chain.
type callChainResponse struct {
	Nodes           []nodeDTO       `json:"nodes"`
	Edges           []edgeDTO       `json:"edges"`
	CrossRepoEdges  []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	IncludedStaging bool            `json:"included_staging"`
	DegradedReasons []string        `json:"degraded_reasons"`
	// IndexingRepos: see GraphResponse.IndexingRepos.
	IndexingRepos []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos: see GraphResponse.WakeReconcilingRepos.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// ScanTrackerReader defines the minimal interface for tracking active background scans.
type ScanTrackerReader interface {
	IsAnyScanRunning() bool
	Snapshot() []application.ScanState
}

// indexingRepoIDs returns active scanning repositories and whether any scan is running.
func indexingRepoIDs(t ScanTrackerReader) ([]string, bool) {
	if t == nil || !t.IsAnyScanRunning() {
		return nil, false
	}
	snap := t.Snapshot()
	if len(snap) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(snap))
	for _, s := range snap {
		ids = append(ids, s.RepoID)
	}
	return ids, true
}

// ReconcileReader defines the interface for checking repository reconciliation status.
type ReconcileReader interface {
	IsRepoReconciling(repoID string) bool
}

// reconcilingForRepos returns the list of queried repositories that are undergoing wake reconciliation.
func reconcilingForRepos(t ReconcileReader, queried []string) []string {
	if t == nil || len(queried) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(queried))
	var out []string
	for _, id := range queried {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if t.IsRepoReconciling(id) {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// ResolveFunc resolves outbound cross-repo edge stubs.
type ResolveFunc func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error)

// InboundResolveFunc resolves inbound cross-repo edge stubs.
type InboundResolveFunc func(ctx context.Context, dstNodeID, branch string) ([]ports.ResolvedEdge, error)

// GraphToolOption configures RegisterGraphTools.
type GraphToolOption func(*graphToolConfig)

type graphToolConfig struct {
	resolve        ResolveFunc
	resolveInbound InboundResolveFunc
	repos          application.RepoLister
	scans          ScanTrackerReader
	reconcile      ReconcileReader
}

// WithResolveFunc supplies a ResolveFunc to enable cross-repo edge resolution in call chains.
func WithResolveFunc(fn ResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolve = fn }
}

// WithInboundResolveFunc supplies an InboundResolveFunc to resolve inbound call chains across repositories.
func WithInboundResolveFunc(fn InboundResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolveInbound = fn }
}

// WithRepoLister supplies the repository registry to resolve relative paths.
func WithRepoLister(repos application.RepoLister) GraphToolOption {
	return func(c *graphToolConfig) { c.repos = repos }
}

// WithScanTracker supplies the background scan tracker to diagnose empty query results.
func WithScanTracker(t ScanTrackerReader) GraphToolOption {
	return func(c *graphToolConfig) { c.scans = t }
}

// WithReconcileTracker supplies the reconciler tracker to identify repositories undergoing active reconciliation.
func WithReconcileTracker(t ReconcileReader) GraphToolOption {
	return func(c *graphToolConfig) { c.reconcile = t }
}

// RegisterGraphTools registers graph query tools in the registry.
func RegisterGraphTools(r *Registry, graph ports.GraphReader, staging *staging.Area, opts ...GraphToolOption) {
	var cfg graphToolConfig
	for _, o := range opts {
		o(&cfg)
	}
	resolve := cfg.resolve
	r.MustRegister(ToolSpec{
		Name:            "eng_find_symbol",
		Description:     "Look up nodes by exact symbol name. Use when you already know the identifier (e.g. 'ParseConfig'). " + DescFindSymbolMatching + " Returns a stable node_id you can feed to eng_get_call_chain, eng_get_blast_radius, eng_get_context_pack, eng_find_duplicates without another lookup. Prefer this over eng_search_semantic for known-identifier queries - it's deterministic and exact.",
		IncludesStaging: true,
		Tier:            Tier1,
		InputSchema:     findSymbolInputSchema,
		Handler:         makeFindSymbolHandler(graph, staging, cfg.repos, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_node",
		Description:     "Get a single node by its ID. node_id is a content-hashed sha256 and globally unique, so repo_id and branch are optional - when omitted the lookup scans across all (repo, branch) pairs. Pass both to apply the staging overlay (only the scoped path can observe an uncommitted staged version).",
		IncludesStaging: true,
		InputSchema:     getNodeInputSchema,
		Handler:         makeGetNodeHandler(graph, staging, cfg.repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_call_chain",
		Description:     DescCallChain,
		IncludesStaging: false,
		InputSchema:     getCallChainInputSchema,
		Handler:         makeGetCallChainHandler(graph, resolve, cfg.resolveInbound, cfg.repos, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_find_implementations",
		Description:     "Given an interface, returns the concrete types that satisfy it; given a concrete type, returns the interfaces it implements. Direction is inferred from the seed kind, so no direction param is needed. Pass node_id (exact) or symbol (resolved via eng_find_symbol; ambiguity rejected). Backed by IMPLEMENTS edges resolved from Go method sets; edges carry a confidence (Definite/Strong/Probable) reflecting how the methods matched.",
		IncludesStaging: false,
		Tier:            Tier1,
		InputSchema:     findImplementationsInputSchema,
		Handler:         makeFindImplementationsHandler(graph, cfg.repos, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_type_hierarchy",
		Description:     "Walk the type hierarchy (IMPLEMENTS + EMBEDS edges) around a type, both directions, depth-bounded. Use to see what a type embeds, what embeds it, and which interfaces sit above or below it in one query. Pass node_id (exact) or symbol (resolved via eng_find_symbol; ambiguity rejected).",
		IncludesStaging: false,
		InputSchema:     getTypeHierarchyInputSchema,
		Handler:         makeGetTypeHierarchyHandler(graph, cfg.repos, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_trace_path",
		Description:     "Trace how one symbol reaches another: returns the shortest path(s) from a source to a target over the chosen edge kinds (CALLS by default). Answers \"does this handler ever reach that DB write\" - the directed point-to-point question eng_get_call_chain (flood) and eng_get_blast_radius (closure) cannot. Pass from_node_id/from_symbol and to_node_id/to_symbol; include IMPLEMENTS in edge_kinds to hop interface to implementer. Empty paths with a reason means no route within the depth bound (not an error).",
		IncludesStaging: false,
		Tier:            Tier1,
		InputSchema:     tracePathInputSchema,
		Handler:         makeTracePathHandler(graph, cfg.repos, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_file_nodes",
		Description:     "Return all nodes for a file path (absolute, or repo-relative when repo_id is given); staged nodes take precedence when available.",
		IncludesStaging: true,
		InputSchema:     getFileNodesInputSchema,
		Handler:         makeGetFileNodesHandler(graph, staging, cfg.repos),
	})
}
