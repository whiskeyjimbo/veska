// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// FindingQuerier is the read-side port to check if a node has any open findings.
// It is distinct from FindingStorage and TodoQuerier to avoid coupling display
// concerns of entry points to those ports. Implementations must scope the
// read to (repoID, branch) because node IDs are only unique within a branch.
type FindingQuerier interface {
	// OpenFindingNodeIDs returns the set of node IDs that have at least one open
	// finding in the given branch. Findings with a null node ID are omitted.
	OpenFindingNodeIDs(ctx context.Context, repoID, branch string) (map[string]bool, error)
}
