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
}

// GraphResponse is the envelope returned by the node-list graph tools
// (eng_find_symbol, eng_get_node, eng_get_file_nodes). Nodes is always a
// non-nil slice so an empty result serializes as [] rather than being
// omitted (solov2-elt).
type GraphResponse struct {
	Nodes           []nodeDTO `json:"nodes"`
	IncludedStaging bool      `json:"included_staging"`
	DegradedReasons []string  `json:"degraded_reasons"`
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
}

// DegradedReasonChainedSelectorsUnresolved is emitted on eng_get_call_chain
// responses when the seed node is a callable (function/method) but no
// CALLS edges were resolvable. The most common cause is chained-selector
// call sites (e.g. cobra's `rootCmd.PersistentFlags().StringVarP(...)`),
// which the tree-sitter extractor does not yet model as edges — see the
// epic at solov2-9rc2. Agents should treat an empty edges array on a
// callable as "parser limitation, may not be authoritative."
const DegradedReasonChainedSelectorsUnresolved = "chained_selectors_unresolved"

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
		Handler:         makeFindSymbolHandler(graph, staging, cfg.repos),
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
		Description:     "Walk CALLS edges from a symbol. Use this — not search — when the question is 'what does this reach' (direction=out, default) or 'what calls this' (direction=in). Surfaces cross_repo_edges into other registered repos so library-symbol callers in a multi-repo workspace are visible without separate queries. Pass node_id (exact) or symbol (resolved via eng_find_symbol; ambiguity is rejected). NOTE: calls inside anonymous functions assigned to struct fields (e.g. cobra Command{Run: func(...){...}}) are not yet captured by the parser — a 'chained_selectors_unresolved' degraded_reason marks this case; fall back to eng_search_semantic or eng_find_symbol.",
		IncludesStaging: false,
		InputSchema:     getCallChainInputSchema,
		Handler:         makeGetCallChainHandler(graph, resolve, cfg.resolveInbound, cfg.repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_file_nodes",
		Description:     "Return all nodes for a file path (absolute, or repo-relative when repo_id is given); staged nodes take precedence when available.",
		IncludesStaging: true,
		InputSchema:     getFileNodesInputSchema,
		Handler:         makeGetFileNodesHandler(graph, staging, cfg.repos),
	})
}
