// SPDX-License-Identifier: AGPL-3.0-only

package ports

// ResolvedEdge is the result of a cross-repository edge resolution. It is defined
// in ports so application-layer consumers can reference the DTO without importing
// adapter packages.
type ResolvedEdge struct {
	SrcNodeID string
	DstNodeID string
	DstRepoID string
	DstBranch string
	Kind      string
	CrossRepo bool
	// SrcLine is the 1-indexed source line of the call expression. It is carried
	// through from cross_repo_edge_stubs so renderers can attribute the edge to the
	// actual call site rather than the caller node's declaration line, where 0
	// represents unknown.
	SrcLine int
}
