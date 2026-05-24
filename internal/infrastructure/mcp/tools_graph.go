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
	DegradedReasons []string  `json:"degraded_reasons,omitempty"`
}

// callChainResponse is the envelope returned by eng_get_call_chain. Both
// nodes and edges are always non-nil so a chain with no reachable callees
// serializes as {"nodes":[],"edges":[]} (solov2-elt).
type callChainResponse struct {
	Nodes           []nodeDTO       `json:"nodes"`
	Edges           []edgeDTO       `json:"edges"`
	CrossRepoEdges  []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	IncludedStaging bool            `json:"included_staging"`
}

// ResolveFunc is a function that resolves cross-repo edge stubs for a given
// node. It is injected into RegisterGraphTools as an optional dependency.
// If nil, cross-repo resolution is skipped.
type ResolveFunc func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error)

// GraphToolOption configures RegisterGraphTools.
type GraphToolOption func(*graphToolConfig)

type graphToolConfig struct {
	resolve ResolveFunc
	repos   application.RepoLister
}

// WithResolveFunc supplies a ResolveFunc that enables cross-repo synthetic
// edge resolution in eng_get_call_chain. Without it, call-chain traversal is
// same-repo only.
func WithResolveFunc(fn ResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolve = fn }
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
		Description:     "Find nodes by symbol name, with staging overlay for in-progress changes.",
		IncludesStaging: true,
		Handler:         makeFindSymbolHandler(graph, staging, cfg.repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_node",
		Description:     "Get a single node by its ID. node_id is a content-hashed sha256 and globally unique, so repo_id and branch are optional — when omitted the lookup scans across all (repo, branch) pairs. Pass both to apply the staging overlay (only the scoped path can observe an uncommitted staged version).",
		IncludesStaging: true,
		Handler:         makeGetNodeHandler(graph, staging, cfg.repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_call_chain",
		Description:     "BFS traversal of CALLS edges up to a configurable depth from a start node. NOTE: calls inside anonymous functions assigned to struct fields (e.g. cobra Command{Run: func(...){...}} var initializers) are not currently captured by the tree-sitter extractor and will not appear as edges — falling back to eng_search_semantic or eng_find_symbol is recommended for that pattern (solov2-vkmi).",
		IncludesStaging: false,
		Handler:         makeGetCallChainHandler(graph, resolve, cfg.repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_file_nodes",
		Description:     "Return all nodes for a file path (absolute, or repo-relative when repo_id is given); staged nodes take precedence when available.",
		IncludesStaging: true,
		Handler:         makeGetFileNodesHandler(graph, staging, cfg.repos),
	})
}
