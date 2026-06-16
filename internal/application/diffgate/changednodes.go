package diffgate

import (
	"context"
	"sort"
)

// nodeHasher is the optional capability the base graph exposes to report a
// node's promoted content hash. sqlite's graph store implements it (the same
// method blastradius.DirtyOf uses to filter unchanged staged nodes). When the
// base does not implement it, every overlay node is treated as changed.
type nodeHasher interface {
	NodeContentHash(ctx context.Context, repoID, branch, nodeID string) (string, error)
}

// ChangedNodeIDs returns the node IDs the candidate actually changed: the
// overlay's nodes minus those a content-hash comparison positively proves
// unchanged. It is the guard's input — the set checked against the finding's
// blast radius.
// Safe-direction bias (this set gates scope-containment): a node is dropped
// ONLY when the base reports a non-empty hash that equals the candidate's. Any
// uncertainty — base has no hasher, the lookup errors, either hash is empty
// leaves the node IN the changed set. Over-reporting makes the guard check more
// nodes against the radius (over-block), which is safe; excluding-on-uncertainty
// would silently shrink the checked set and let a far-reaching change slip
// through as "contained".
// Removed nodes are not returned (a deleted file stages an empty overlay entry,
// so it contributes no candidate node); scope-containment is about nodes the
// change touched, which for a deletion is captured by the file's surviving
// neighbours, not the vanished node.
func (e *Ephemeral) ChangedNodeIDs(ctx context.Context) []string {
	hasher, hasHasher := e.Base.(nodeHasher)
	seen := make(map[string]struct{})
	var ids []string
	for _, f := range e.Overlay.Snapshot(e.RepoID, e.Branch) {
		for _, n := range f.Nodes {
			if n == nil {
				continue
			}
			id := string(n.ID)
			if _, dup := seen[id]; dup {
				continue
			}
			if hasHasher && n.ContentHash != nil {
				baseHash, err := hasher.NodeContentHash(ctx, e.RepoID, e.Branch, id)
				if err == nil && baseHash != "" && baseHash == string(*n.ContentHash) {
					// Positively confirmed unchanged — the only case we drop.
					continue
				}
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
