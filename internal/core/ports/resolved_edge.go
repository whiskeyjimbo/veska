package ports

// ResolvedEdge is the result of a successful one-hop cross-repo edge
// resolution. CrossRepo is always true for edges produced by a resolver.
//
// Defined in core/ports so application-layer consumers (e.g. MCP tools) can
// reference the DTO without importing the SQLite adapter package.
type ResolvedEdge struct {
	SrcNodeID string
	DstNodeID string
	DstRepoID string
	DstBranch string
	Kind      string
	CrossRepo bool
	// SrcLine is the 1-indexed source line of the call_expression in the
	// SrcNodeID's file. Carried through from cross_repo_edge_stubs.src_line
	// so renderers can attribute the cross-repo edge to the actual call
	// site rather than the caller node's declaration line (solov2-izh6.31).
	// 0 means unknown (pre-migration stubs).
	SrcLine int
}
