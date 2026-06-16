package mcp

import (
	"context"
	"sort"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// CrossRepoEdge represents a synthetic edge that crosses repository boundaries.
// CrossRepo is always true for edges produced by the resolver.
type CrossRepoEdge struct {
	SrcNodeID string `json:"src_node_id"`
	DstNodeID string `json:"dst_node_id"`
	DstRepoID string `json:"dst_repo_id"`
	DstBranch string `json:"dst_branch"`
	Kind      string `json:"kind"`
	CrossRepo bool   `json:"cross_repo"` // always true
	// SrcLine is the 1-indexed line of the call_expression inside the
	// source node's file. Renderers prefer this over the caller node's
	// declaration line when set so a function with N cross-repo calls
	// shows each at its actual call site. Omitted when
	// unknown (pre-migration stubs or non-Go languages without the
	// adoption).
	SrcLine int `json:"src_line,omitempty"`
}

// GraphResponse is the envelope returned by the node-list graph tools
// (eng_find_symbol, eng_get_node, eng_get_file_nodes). Nodes is always a
// non-nil slice so an empty result serializes as rather than being
// omitted.
type GraphResponse struct {
	Nodes           []nodeDTO `json:"nodes"`
	IncludedStaging bool      `json:"included_staging"`
	DegradedReasons []string  `json:"degraded_reasons"`
	// IndexingRepos lists repo_ids for which a cold scan was still in flight
	// at query time. Populated only when DegradedReasons contains
	// "indexing_in_progress" so callers can decide whether their target
	// repo is the one being indexed. Omitted from JSON when empty
	IndexingRepos []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos lists repo_ids touched by this query that had an
	// in-flight wake reconcile sweep at query time. Populated only when
	// DegradedReasons contains "wake_reconciling". Omitted when empty.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// callChainResponse is the envelope returned by eng_get_call_chain. Both
// nodes and edges are always non-nil so a chain with no reachable callees
// serializes as {"nodes":,"edges":}. DegradedReasons
// carries advisory hints — e.g. "chained_selectors_unresolved" when the
// seed is callable but no CALLS edges resolved — so an
// agent reading the response knows the empty result may reflect a parser
// limitation rather than a symbol with no callees.
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

// ScanTrackerReader is the minimal read surface mcp tool handlers need
// from application.ScanTracker. Defined as an interface here so test
// fixtures can stub it without pulling in the application package's
// concrete tracker, and so handlers gracefully no-op when no tracker
// has been wired (nil-safe everywhere).
type ScanTrackerReader interface {
	IsAnyScanRunning() bool
	Snapshot() []application.ScanState
}

// indexingRepoIDs returns the sorted list of repo_ids with a cold scan
// in flight at call time, plus the boolean "any scan running" used to
// decide whether to attach the indexing_in_progress degraded reason.
// Nil-safe: a nil tracker yields (nil, false), so callers that didn't
// wire WithScanTracker keep their pre-existing behaviour.
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

// ReconcileReader is the minimal read surface graph tool handlers need from
// the wake reconciler (git.WakeReconciler) to attach a wake_reconciling
// degraded reason. Declared in the consumer (mcp) package so test fixtures can
// stub it and so the git infrastructure layer is not imported here. Nil-safe:
// a nil reader yields no reconciling repos.
type ReconcileReader interface {
	IsRepoReconciling(repoID string) bool
}

// reconcilingForRepos returns the sorted subset of queried repo_ids whose wake
// sweep is in flight. queried is the set of repos the caller's result touches
// (request repo_id or, when repo-agnostic, the result repo_ids). Unlike
// indexingRepoIDs this is NOT gated on empty results — a sweep may be
// re-parsing files a non-empty response just read. Nil-safe: a nil reader or
// empty queried set yields nil.
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

// ResolveFunc is a function that resolves cross-repo edge stubs OUTBOUND
// from a given node (the node is the caller). Injected as an optional
// dependency; nil = skip outbound resolution.
type ResolveFunc func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error)

// InboundResolveFunc resolves cross-repo edge stubs INBOUND to a given
// node (the node is the callee). Use it to answer "who calls this library
// symbol from another repo?" — the dual of ResolveFunc. Backed by
// resolver.ResolveStubsTargetingNode. nil = skip inbound
// resolution.
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

// WithResolveFunc supplies a ResolveFunc that enables cross-repo synthetic
// edge resolution in eng_get_call_chain. Without it, call-chain traversal is
// same-repo only.
func WithResolveFunc(fn ResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolve = fn }
}

// WithInboundResolveFunc supplies an InboundResolveFunc so call_chain
// direction=in (and direction=both) surfaces callers in OTHER repos
// closes the parity gap with eng_get_blast_radius for library symbols
func WithInboundResolveFunc(fn InboundResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolveInbound = fn }
}

// WithRepoLister supplies the repos registry so eng_get_file_nodes can resolve
// a repo-relative file_path against the repo's root. Node file paths are stored
// absolute; without this, a relative path silently matched nothing.
func WithRepoLister(repos application.RepoLister) GraphToolOption {
	return func(c *graphToolConfig) { c.repos = repos }
}

// WithScanTracker supplies the daemon-wide cold-scan tracker so empty
// graph-read responses can carry an "indexing_in_progress" degraded reason
// when the empty result was likely caused by a scan still in flight rather
// than the symbol genuinely not existing. Nil is allowed
// and disables the hint (matches single-process tests with no daemon).
func WithScanTracker(t ScanTrackerReader) GraphToolOption {
	return func(c *graphToolConfig) { c.scans = t }
}

// WithReconcileTracker supplies the wake reconciler so graph read responses can
// carry a "wake_reconciling" degraded reason while a queried repo's
// suspend/resume mtime sweep is in flight. Nil is allowed and disables the hint.
func WithReconcileTracker(t ReconcileReader) GraphToolOption {
	return func(c *graphToolConfig) { c.reconcile = t }
}

// RegisterGraphTools registers the 5 graph read tools on r.
// graph and staging are injected dependencies; pass WithResolveFunc to enable
// cross-repo synthetic edge resolution in eng_get_call_chain.
func RegisterGraphTools(r *Registry, graph ports.GraphReader, staging *staging.Area, opts ...GraphToolOption) {
	var cfg graphToolConfig
	for _, o := range opts {
		o(&cfg)
	}
	resolve := cfg.resolve
	r.MustRegister(ToolSpec{
		Name:            "eng_find_symbol",
		Description:     "Look up nodes by exact symbol name. Use when you already know the identifier (e.g. 'ParseConfig'). " + DescFindSymbolMatching + " Returns a stable node_id you can feed to eng_get_call_chain, eng_get_blast_radius, eng_get_context_pack, eng_search_similar without another lookup. Prefer this over eng_search_semantic for known-identifier queries — it's deterministic and exact.",
		IncludesStaging: true,
		InputSchema:     findSymbolInputSchema,
		Handler:         makeFindSymbolHandler(graph, staging, cfg.repos, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_node",
		Description:     "Get a single node by its ID. node_id is a content-hashed sha256 and globally unique, so repo_id and branch are optional — when omitted the lookup scans across all (repo, branch) pairs. Pass both to apply the staging overlay (only the scoped path can observe an uncommitted staged version).",
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
		Name:            "eng_get_file_nodes",
		Description:     "Return all nodes for a file path (absolute, or repo-relative when repo_id is given); staged nodes take precedence when available.",
		IncludesStaging: true,
		InputSchema:     getFileNodesInputSchema,
		Handler:         makeGetFileNodesHandler(graph, staging, cfg.repos),
	})
}
