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
	// (the anchor plus its blast radius, expanded by new nodes wired into it).
	Contained bool `json:"contained"`
	// AnchorNodeID is the finding anchor the radius was computed from.
	AnchorNodeID string `json:"anchor_node_id"`
	// Offending lists the changed node IDs that fell OUTSIDE the allowed set,
	// sorted for deterministic output. Empty when Contained.
	Offending []string `json:"offending"`
	// Truncated is propagated from the radius traversal: when the BFS hit its
	// node bound the allowed set is incomplete, so an "exceeded" verdict may
	// be a false positive. Callers can surface this as a caveat.
	Truncated bool `json:"truncated"`
}

// Guard answers the blast-radius-containment half of the diff-safety gate: did
// a candidate change stay within the anchor's blast radius? It distinguishes
// "modified EXISTING distant code" (scope creep — offending) from "NEW code
// wired into the allowed neighbourhood" (the fix's natural footprint —
// contained), so the canonical fix that adds a caller of a dead symbol is not
// over-blocked (solov2-ll57.5). It is stateless and safe for concurrent
// callers.
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

// Check reports whether the candidate's changed nodes stay within the anchor's
// blast radius. The allowed set starts as {anchor ∪ radius(anchor)} over the
// base graph, then is expanded with NEW nodes (absent in the base) that the
// candidate wires — via a resolved overlay edge — into the allowed set; the
// expansion is run to a fixpoint so a chain of new nodes connected to the fix
// is admitted. A changed node outside the resulting set is offending.
//
// opts selects the radius policy (depth/direction/bounds); the zero value uses
// the blastradius defaults. The Guard does no network IO — it reads the base
// radius and the in-memory ephemeral overlay.
//
// Safe/unsafe asymmetry: when membership or connectivity can't be determined
// (a base lookup error, an unresolved edge), the node stays OFFENDING
// (over-block) rather than being admitted — a false "exceeded" over-blocks a
// good change, but never lets scope creep pass.
func (g *Guard) Check(ctx context.Context, eph *Ephemeral, anchorNodeID string, opts blastradius.Options) (ScopeVerdict, error) {
	if eph == nil {
		return ScopeVerdict{}, fmt.Errorf("%w: ephemeral graph is nil", ErrMissingDependency)
	}
	if anchorNodeID == "" {
		return ScopeVerdict{}, fmt.Errorf("diffgate: guard requires a node-anchored finding (empty anchor)")
	}
	resp, err := g.radius.Of(ctx, eph.RepoID, eph.Branch, []string{anchorNodeID}, opts)
	if err != nil {
		return ScopeVerdict{}, fmt.Errorf("diffgate: blast radius of anchor %q: %w", anchorNodeID, err)
	}
	// Allowed = anchor ∪ radius. The anchor is added explicitly even though the
	// service returns it as the distance-0 seed, so the set is correct
	// regardless of that detail.
	allowed := make(map[string]struct{}, len(resp.Entries)+1)
	allowed[anchorNodeID] = struct{}{}
	for _, e := range resp.Entries {
		allowed[e.NodeID] = struct{}{}
	}

	changed := eph.ChangedNodeIDs(ctx)
	newNodes, err := g.newNodes(ctx, eph, changed)
	if err != nil {
		return ScopeVerdict{}, err
	}
	g.admitNewWired(allowed, newNodes, eph)

	var offending []string
	for _, id := range changed {
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

// newNodes returns the subset of changed node IDs that are absent from the base
// graph (i.e. introduced by the candidate). Membership is decided by the base
// NodeLookup: IDs it can hydrate exist; the rest are new. On a lookup error the
// safe choice is to treat NOTHING as new (no expansion → over-block), so the
// error is returned and the caller fails closed.
func (g *Guard) newNodes(ctx context.Context, eph *Ephemeral, changed []string) (map[string]struct{}, error) {
	if len(changed) == 0 {
		return map[string]struct{}{}, nil
	}
	metas, err := eph.Base.LookupNodes(ctx, eph.RepoID, eph.Branch, changed)
	if err != nil {
		return nil, fmt.Errorf("diffgate: guard base membership lookup: %w", err)
	}
	existing := make(map[string]struct{}, len(metas))
	for _, m := range metas {
		existing[m.NodeID] = struct{}{}
	}
	out := make(map[string]struct{})
	for _, id := range changed {
		if _, ok := existing[id]; !ok {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// admitNewWired adds to allowed every new node connected — through a resolved
// candidate edge, in either direction — to a node already in allowed, repeated
// to a fixpoint so a chain of new nodes wired to the fix is admitted. Only NEW
// nodes are admitted this way: a modified EXISTING node outside the radius is
// real scope creep and is never expanded in.
func (g *Guard) admitNewWired(allowed, newNodes map[string]struct{}, eph *Ephemeral) {
	if len(newNodes) == 0 {
		return
	}
	// Undirected adjacency over resolved overlay edges, restricted to endpoints
	// we care about. An unresolved edge has no bound target, so it cannot
	// establish connectivity — leaving its new node offending (over-block).
	adj := make(map[string][]string)
	for _, f := range eph.Overlay.Snapshot(eph.RepoID, eph.Branch) {
		for _, e := range f.Edges {
			if e == nil || !e.Resolved {
				continue
			}
			src, tgt := string(e.Src), string(e.Tgt)
			if src == "" || tgt == "" {
				continue
			}
			adj[src] = append(adj[src], tgt)
			adj[tgt] = append(adj[tgt], src)
		}
	}
	for {
		grew := false
		for n := range newNodes {
			if _, ok := allowed[n]; ok {
				continue
			}
			for _, nbr := range adj[n] {
				if _, ok := allowed[nbr]; ok {
					allowed[n] = struct{}{}
					grew = true
					break
				}
			}
		}
		if !grew {
			return
		}
	}
}
