// SPDX-FileCopyrightText: 2026 Jeff Rose
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
type EdgeReader interface {
	// InboundEdges returns, for each node ID, the source node IDs where the
	// queried node is the destination. Conceptually: who references these nodes.
	InboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)

	// OutboundEdges returns, for each node ID, the destination node IDs of
	// edges originating from that node. Conceptually: what do these nodes reference.
	OutboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)
}
