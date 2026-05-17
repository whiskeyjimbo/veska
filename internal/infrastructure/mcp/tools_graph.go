package mcp

import (
	"context"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
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

// GraphResponse is the standard envelope returned by all graph read tools.
type GraphResponse struct {
	Nodes           []*domain.Node  `json:"nodes,omitempty"`
	Edges           []*domain.Edge  `json:"edges,omitempty"`
	CrossRepoEdges  []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	IncludedStaging bool            `json:"included_staging"`
	DegradedReasons []string        `json:"degraded_reasons,omitempty"`
}

// ResolveFunc is a function that resolves cross-repo edge stubs for a given
// node. It is injected into RegisterGraphTools as an optional dependency.
// If nil, cross-repo resolution is skipped.
type ResolveFunc func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error)

// GraphToolOption configures RegisterGraphTools.
type GraphToolOption func(*graphToolConfig)

type graphToolConfig struct {
	resolve ResolveFunc
}

// WithResolveFunc supplies a ResolveFunc that enables cross-repo synthetic
// edge resolution in eng_get_call_chain. Without it, call-chain traversal is
// same-repo only.
func WithResolveFunc(fn ResolveFunc) GraphToolOption {
	return func(c *graphToolConfig) { c.resolve = fn }
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
		Handler:         makeFindSymbolHandler(graph, staging),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_node",
		Description:     "Get a single node by its ID, with staging overlay applied.",
		IncludesStaging: true,
		Handler:         makeGetNodeHandler(graph, staging),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_call_chain",
		Description:     "BFS traversal of CALLS edges up to a configurable depth from a start node.",
		IncludesStaging: false,
		Handler:         makeGetCallChainHandler(graph, resolve),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_file_nodes",
		Description:     "Return all nodes for a file path; staged nodes take precedence when available.",
		IncludesStaging: true,
		Handler:         makeGetFileNodesHandler(graph, staging),
	})
}
