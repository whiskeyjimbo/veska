package ports

import "context"

// FindingQuerier is the read-side port for asking "does this node carry
// any open findings". The wiki entry_points surface uses it to exclude
// symbols that are not currently safe starting points.
// It is intentionally distinct from FindingStorage (write-side) and from
// TodoQuerier (a single-rule projection): entry_points needs a presence
// check over arbitrary rules, scoped per node, so widening either of the
// existing ports would couple them to this display concern.
// Implementations sit on top of the findings table and must scope the
// read to (repoID, branch); a node_id is only unique within a branch.
type FindingQuerier interface {
	// OpenFindingNodeIDs returns the set of node_id values that have at
	// least one open finding (state='open') in (repoID, branch). Findings
	// with a NULL node_id are omitted. The result is a set: each node_id
	// appears once, mapped to true.
	OpenFindingNodeIDs(ctx context.Context, repoID, branch string) (map[string]bool, error)
}
