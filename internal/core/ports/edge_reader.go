package ports

import "context"

// EdgeReader is the read-side companion to EdgeStorage. It exposes batch
// reverse and forward adjacency queries over the edges table for callers
// that need to walk the graph without loading it entirely into memory
// (e.g. the blast-radius BFS service).
// Both methods take a batch of node IDs and return a map keyed by the
// queried node ID. IDs with no matching edges are returned with an
// empty/nil slice rather than omitted from the map — callers can rely
// on a present key meaning "queried".
// Implementations must scope the read to (repoID, branch); edges are
// only meaningful within a branch.
type EdgeReader interface {
	// InboundEdges returns, for each node_id in nodeIDs, the list of
	// src_node_id values for edges with that node as the destination.
	// Conceptually: "who calls / references these nodes".
	InboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)

	// OutboundEdges returns, for each node_id in nodeIDs, the list of
	// dst_node_id values for edges originating from that node.
	// Conceptually: "what do these nodes call / reference".
	OutboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)
}
