package mcp

import (
	"context"

	application "github.com/whiskeyjimbo/veska/internal/application"
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
	// shows each at its actual call site (solov2-izh6.31). Omitted when
	// unknown (pre-migration stubs or non-Go languages without the
	// adoption).
	SrcLine int `json:"src_line,omitempty"`
}

// GraphResponse is the envelope returned by the node-list graph tools
// (eng_find_symbol, eng_get_node, eng_get_file_nodes). Nodes is always a
// non-nil slice so an empty result serializes as [] rather than being
// omitted (solov2-elt).
type GraphResponse struct {
	Nodes           []nodeDTO `json:"nodes"`
	IncludedStaging bool      `json:"included_staging"`
	DegradedReasons []string  `json:"degraded_reasons"`
	// IndexingRepos lists repo_ids for which a cold scan was still in flight
	// at query time. Populated only when DegradedReasons contains
	// "indexing_in_progress" so callers can decide whether their target
	// repo is the one being indexed. Omitted from JSON when empty
	// (solov2-izh6.30).
	IndexingRepos []string `json:"indexing_repos,omitempty"`
}

// callChainResponse is the envelope returned by eng_get_call_chain. Both
// nodes and edges are always non-nil so a chain with no reachable callees
// serializes as {"nodes":[],"edges":[]} (solov2-elt). DegradedReasons
// carries advisory hints — e.g. "chained_selectors_unresolved" when the
// seed is callable but no CALLS edges resolved (solov2-jojv) — so an
// agent reading the response knows the empty result may reflect a parser
// limitation rather than a symbol with no callees.
type callChainResponse struct {
	Nodes           []nodeDTO       `json:"nodes"`
	Edges           []edgeDTO       `json:"edges"`
	CrossRepoEdges  []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	IncludedStaging bool            `json:"included_staging"`
	DegradedReasons []string        `json:"degraded_reasons"`
	// IndexingRepos: see GraphResponse.IndexingRepos (solov2-izh6.30).
	IndexingRepos []string `json:"indexing_repos,omitempty"`
}

// DegradedReasonChainedSelectorsUnresolved is emitted on eng_get_call_chain
// responses when the seed node is a callable whose body contains chained
// selector call sites (e.g. cobra's `rootCmd.PersistentFlags().StringVarP(...)`,
// or `s.field.M()`) that the tree-sitter extractor does not yet model as
// edges — see epic solov2-9rc2. Agents should treat an empty edges array
// on a callable carrying this reason as "parser limitation, may not be
// authoritative."
const DegradedReasonChainedSelectorsUnresolved = "chained_selectors_unresolved"

// DegradedReasonExternalCalleesOnly is emitted when the seed callable's
// body has no chained selectors but also produced no resolvable CALLS
// edges. The dominant cause is that every callee lives outside the
// indexed graph (stdlib like fmt/strings, or third-party packages from
// unregistered modules). An agent reading this should NOT conclude the
// parser is buggy — the empty edges set reflects the index boundary,
// not a parser limitation (solov2-izh6.22).
const DegradedReasonExternalCalleesOnly = "external_callees_only"

// DegradedReasonIndexingInProgress is emitted on any read tool that
// returned an empty result while at least one cold scan was still
// running. A query that hits the daemon during the cold-scan window
// would otherwise see {nodes:[]} silently and conclude the symbol does
// not exist; this reason tells the caller to retry once indexing
// settles (solov2-izh6.30). The accompanying IndexingRepos field, when
// populated, lists the repo_ids the caller should wait on.
const DegradedReasonIndexingInProgress = "indexing_in_progress"

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

// ResolveFunc is a function that resolves cross-repo edge stubs OUTBOUND
// from a given node (the node is the caller). Injected as an optional
// dependency; nil = skip outbound resolution.
type ResolveFunc func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error)

// InboundResolveFunc resolves cross-repo edge stubs INBOUND to a given
// node (the node is the callee). Use it to answer "who calls this library
// symbol from another repo?" — the dual of ResolveFunc. Backed by
// resolver.ResolveStubsTargetingNode (solov2-80hh). nil = skip inbound
// resolution.
type InboundResolveFunc func(ctx context.Context, dstNodeID, branch string) ([]ports.ResolvedEdge, error)

// GraphToolOption configures RegisterGraphTools.
type GraphToolOption func(*graphToolConfig)

type graphToolConfig struct {
	resolve        ResolveFunc
	resolveInbound InboundResolveFunc
	repos          application.RepoLister
	scans          ScanTrackerReader
}

// WithResolveFunc supplies a ResolveFunc that enables cross-repo synthetic
// edge resolution in eng_get_call_chain. Without it, call-chain traversal is
// same-repo only.
func WithResolveFunc(fn ResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolve = fn }
}

// WithInboundResolveFunc supplies an InboundResolveFunc so call_chain
// direction=in (and direction=both) surfaces callers in OTHER repos —
// closes the parity gap with eng_get_blast_radius for library symbols
// (solov2-80hh).
func WithInboundResolveFunc(fn InboundResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolveInbound = fn }
}

// WithRepoLister supplies the repos registry so eng_get_file_nodes can resolve
// a repo-relative file_path against the repo's root. Node file paths are stored
// absolute; without this, a relative path silently matched nothing (solov2-829).
func WithRepoLister(repos application.RepoLister) GraphToolOption {
	return func(c *graphToolConfig) { c.repos = repos }
}

// WithScanTracker supplies the daemon-wide cold-scan tracker so empty
// graph-read responses can carry an "indexing_in_progress" degraded reason
// when the empty result was likely caused by a scan still in flight rather
// than the symbol genuinely not existing (solov2-izh6.30). Nil is allowed
// and disables the hint (matches single-process tests with no daemon).
func WithScanTracker(t ScanTrackerReader) GraphToolOption {
	return func(c *graphToolConfig) { c.scans = t }
}

// RegisterGraphTools registers the 5 graph read tools on r.
// graph and staging are injected dependencies; pass WithResolveFunc to enable
// cross-repo synthetic edge resolution in eng_get_call_chain.
func RegisterGraphTools(r *Registry, graph ports.GraphStorage, staging *application.StagingArea, opts ...GraphToolOption) {
	var cfg graphToolConfig
	for _, o := range opts {
		o(&cfg)
	}
	resolve := cfg.resolve
	r.MustRegister(ToolSpec{
		Name:            "eng_find_symbol",
		Description:     "Look up nodes by exact symbol name. Use when you already know the identifier (e.g. 'ParseConfig'). Unqualified names also match — 'Run' finds Server.Run, Command.Run, etc., with exact matches first. Returns a stable node_id you can feed to eng_get_call_chain, eng_get_blast_radius, eng_get_context_pack, eng_search_similar without another lookup. Prefer this over eng_search_semantic for known-identifier queries — it's deterministic and exact.",
		IncludesStaging: true,
		InputSchema:     findSymbolInputSchema,
		Handler:         makeFindSymbolHandler(graph, staging, cfg.repos, cfg.scans),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_node",
		Description:     "Get a single node by its ID. node_id is a content-hashed sha256 and globally unique, so repo_id and branch are optional — when omitted the lookup scans across all (repo, branch) pairs. Pass both to apply the staging overlay (only the scoped path can observe an uncommitted staged version).",
		IncludesStaging: true,
		InputSchema:     getNodeInputSchema,
		Handler:         makeGetNodeHandler(graph, staging, cfg.repos),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_call_chain",
		Description:     DescCallChain,
		IncludesStaging: false,
		InputSchema:     getCallChainInputSchema,
		Handler:         makeGetCallChainHandler(graph, resolve, cfg.resolveInbound, cfg.repos, cfg.scans),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_file_nodes",
		Description:     "Return all nodes for a file path (absolute, or repo-relative when repo_id is given); staged nodes take precedence when available.",
		IncludesStaging: true,
		InputSchema:     getFileNodesInputSchema,
		Handler:         makeGetFileNodesHandler(graph, staging, cfg.repos),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
}
