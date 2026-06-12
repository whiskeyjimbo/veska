package diffgate

import (
	"context"
	"fmt"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
)

// BlastRadius is the ISP-narrowed view of blastradius.Service the Guard needs:
// just "what nodes are reachable from these seeds". *blastradius.Service
// satisfies it directly, so the guard reuses the existing service (h1yb.4 DoD)
// without diffgate owning a second radius implementation.
type BlastRadius interface {
	Of(ctx context.Context, repoID, branch string, seedIDs []string, opts blastradius.Options) (blastradius.Response, error)
}

// The real service is the production implementation; this assertion keeps the
// narrow port honest if blastradius.Service.Of ever changes shape.
var _ BlastRadius = (*blastradius.Service)(nil)

// ScopeVerdict reports whether a candidate change stayed within the blast
// radius of its target finding's anchor. It is a signal, not an action: the
// Guard never blocks or merges — composition into a pass/fail gate is ll57.2.
type ScopeVerdict struct {
	// Contained is true when every changed node falls inside the allowed set
	// (the anchor plus its blast radius).
	Contained bool
	// AnchorNodeID is the finding anchor the radius was computed from.
	AnchorNodeID string
	// Offending lists the changed node IDs that fell OUTSIDE the allowed set,
	// sorted for deterministic output. Empty when Contained.
	Offending []string
	// Truncated is propagated from the radius traversal: when the BFS hit its
	// node bound the allowed set is incomplete, so an "exceeded" verdict may
	// be a false positive. Callers can surface this as a caveat.
	Truncated bool
}

// Guard answers the blast-radius-containment half of the diff-safety gate:
// given a finding's anchor node and the set of nodes a candidate change
// touched, does the change stay within the anchor's blast radius? It is
// stateless and safe for concurrent callers.
type Guard struct {
	radius BlastRadius
}

// NewGuard constructs a Guard over the supplied blast-radius service. The
// service is required.
func NewGuard(radius BlastRadius) (*Guard, error) {
	if radius == nil {
		return nil, fmt.Errorf("%w: blast-radius service is nil", ErrMissingDependency)
	}
	return &Guard{radius: radius}, nil
}

// Check computes the allowed set — the anchor plus every node in its blast
// radius — and reports which of changedNodeIDs, if any, fall outside it. opts
// selects the radius policy (depth/direction/bounds); the zero value uses the
// blastradius service defaults. The changed-node set is supplied by the caller
// (derived from the ephemeral index); the Guard itself does no indexing and no
// network IO.
func (g *Guard) Check(ctx context.Context, repoID, branch, anchorNodeID string, changedNodeIDs []string, opts blastradius.Options) (ScopeVerdict, error) {
	if anchorNodeID == "" {
		return ScopeVerdict{}, fmt.Errorf("diffgate: guard requires a node-anchored finding (empty anchor)")
	}
	resp, err := g.radius.Of(ctx, repoID, branch, []string{anchorNodeID}, opts)
	if err != nil {
		return ScopeVerdict{}, fmt.Errorf("diffgate: blast radius of anchor %q: %w", anchorNodeID, err)
	}
	// Allowed = anchor ∪ radius. The anchor is added explicitly even though
	// the service returns it as the distance-0 seed, so the set is correct
	// regardless of that detail.
	allowed := make(map[string]struct{}, len(resp.Entries)+1)
	allowed[anchorNodeID] = struct{}{}
	for _, e := range resp.Entries {
		allowed[e.NodeID] = struct{}{}
	}
	var offending []string
	for _, id := range changedNodeIDs {
		if id == "" {
			continue
		}
		if _, ok := allowed[id]; !ok {
			offending = append(offending, id)
		}
	}
	sort.Strings(offending)
	return ScopeVerdict{
		Contained:    len(offending) == 0,
		AnchorNodeID: anchorNodeID,
		Offending:    offending,
		Truncated:    resp.Truncated,
	}, nil
}
