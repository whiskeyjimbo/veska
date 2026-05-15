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
}
