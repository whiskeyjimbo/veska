// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// EdgeReader is the read-side companion to EdgeStorage. It exposes batch
// reverse and forward adjacency queries over the edges table for callers
// that need to walk the graph without loading it entirely into memory.
// Both methods take a batch of node IDs and return a map keyed by the
// queried node ID. IDs with no matching edges are returned with an
// empty or nil slice rather than omitted from the map; callers can rely
// on a present key meaning "queried".
// Implementations must scope the read to (repoID, branch) because edges are
// only meaningful within a branch.
//
// Both methods walk STRUCTURAL edges only: advisory edges (domain.IsAdvisory,
// currently SIMILAR_TO) are excluded so impact and reachability callers don't
// bridge unrelated subgraphs through semantically-similar look-alike symbols.
type EdgeReader interface {
	// InboundEdges returns, for each node ID, the source node IDs where the
	// queried node is the destination, over structural edges only.
	// Conceptually: who structurally references these nodes.
	InboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)

	// OutboundEdges returns, for each node ID, the destination node IDs of
	// structural edges originating from that node.
	// Conceptually: what do these nodes structurally reference.
	OutboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)
}
