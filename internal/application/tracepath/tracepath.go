// SPDX-License-Identifier: AGPL-3.0-only

// Package tracepath finds point-to-point reachability paths over the code graph:
// "how does A reach B". It is the engine behind eng_trace_path. Unlike a
// single-source flood (call chain) or a transitive closure (blast radius), it
// answers a directed A->B question and returns the actual connecting path(s).
//
// The search is a bounded BIDIRECTIONAL BFS: a forward frontier grows from the
// source over outgoing edges while a backward frontier grows from the target
// over incoming edges, expanding whichever frontier is smaller each step until
// they meet. On an unweighted graph this yields shortest paths while exploring
// far fewer nodes than a one-directional BFS (roughly b^(d/2) vs b^d). It walks
// an in-memory *domain.Graph because the traversal must filter and report edge
// KINDS, which the batched edge reader does not carry.
package tracepath

import "github.com/whiskeyjimbo/veska/internal/core/domain"

// Options bounds and parameterizes a search. The zero value is not useful;
// callers set sensible defaults (the MCP layer defaults EdgeKinds to CALLS).
type Options struct {
	// EdgeKinds restricts traversal to these edge kinds. Empty means "any kind"
	// - the adapter, not the core, owns the CALLS-by-default policy.
	EdgeKinds []domain.EdgeKind
	// MaxDepth caps the path length (number of edges). A path longer than this is
	// not returned; an exhausted search to this depth reports no path.
	MaxDepth int
	// MaxPaths caps how many distinct shortest paths are returned (>=1).
	MaxPaths int
	// HubDegree: a node with more than this many in-kind neighbors is treated as
	// a hub and not expanded THROUGH (its fan-out would explode the search, e.g.
	// a ubiquitous logger). 0 disables the gate. Mirrors blastradius's hub cap.
	HubDegree int
	// MaxVisited caps total nodes discovered across both frontiers; hitting it
	// stops the search and marks the result truncated.
	MaxVisited int
}

// Path is one connecting route: Nodes[0] is the source, the last is the target,
// and Edges[i] is the edge traversed from Nodes[i] to Nodes[i+1]
// (len(Edges) == len(Nodes)-1).
type Path struct {
	Nodes []domain.NodeID
	Edges []*domain.Edge
}

// Result is the outcome of a search. Paths is empty when none was found within
// the bounds; Reason then explains why. Truncated is set when a bound stopped
// the search before it was exhausted, with Bound naming which one fired.
type Result struct {
	Paths     []Path
	Truncated bool
	Bound     string // "visited" | "hub" | "depth" when Truncated
	Reason    string // set when len(Paths) == 0
}

// half is one direction's BFS bookkeeping. preds maps a node to the (parent,
// edge) pairs on a shortest path back toward this side's origin; multiple
// parents at the same distance enable multi-path reconstruction.
type half struct {
	dist  map[domain.NodeID]int
	preds map[domain.NodeID][]predecessor
	front []domain.NodeID
	level int
}

type predecessor struct {
	node domain.NodeID
	edge *domain.Edge
}

// Find returns up to opts.MaxPaths shortest paths from `from` to `to` over the
// edge kinds in opts, or an empty Result with a Reason when none exists within
// the bounds.
func Find(g *domain.Graph, from, to domain.NodeID, opts Options) Result {
	if opts.MaxPaths < 1 {
		opts.MaxPaths = 1
	}
	if from == to {
		return Result{Paths: []Path{{Nodes: []domain.NodeID{from}}}}
	}
	kinds := kindSet(opts.EdgeKinds)

	fwd := newHalf(from)
	bwd := newHalf(to)
	visited := 2
	res := Result{}

	mu := -1 // best (shortest) meeting total distance found so far; -1 = none
	for len(fwd.front) > 0 && len(bwd.front) > 0 {
		// Stop once neither side can produce a path shorter than the best found.
		if mu >= 0 && fwd.level+bwd.level >= mu {
			break
		}
		// No meet yet and we have reached the depth budget: nothing within range.
		if mu < 0 && fwd.level+bwd.level >= opts.MaxDepth {
			break
		}
		// Expand the smaller frontier (the bidirectional win).
		if len(fwd.front) <= len(bwd.front) {
			expand(g, &fwd, kinds, true, opts, &visited, &res)
		} else {
			expand(g, &bwd, kinds, false, opts, &visited, &res)
		}
		if res.Truncated && res.Bound == "visited" {
			break
		}
		if m := bestMeet(fwd, bwd); m >= 0 && (mu < 0 || m < mu) {
			mu = m
		}
	}

	if mu < 0 || mu > opts.MaxDepth {
		if res.Truncated {
			res.Reason = "search truncated by the " + res.Bound + " bound before a path was found"
		} else {
			res.Reason = "no path within the depth bound"
		}
		return res
	}

	res.Paths = reconstruct(fwd, bwd, from, to, mu, opts.MaxPaths)
	return res
}

func newHalf(origin domain.NodeID) half {
	return half{
		dist:  map[domain.NodeID]int{origin: 0},
		preds: map[domain.NodeID][]predecessor{},
		front: []domain.NodeID{origin},
	}
}

func kindSet(kinds []domain.EdgeKind) map[domain.EdgeKind]struct{} {
	if len(kinds) == 0 {
		return nil // nil => match any kind
	}
	m := make(map[domain.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		m[k] = struct{}{}
	}
	return m
}

// neighbors returns the in-kind (neighbor, edge) pairs of node u in the given
// direction: forward follows outgoing edges to their target, backward follows
// incoming edges to their source.
func neighbors(g *domain.Graph, u domain.NodeID, kinds map[domain.EdgeKind]struct{}, forward bool) []predecessor {
	var edges []*domain.Edge
	if forward {
		edges = g.OutgoingEdges(u)
	} else {
		edges = g.IncomingEdges(u)
	}
	out := make([]predecessor, 0, len(edges))
	for _, e := range edges {
		if kinds != nil {
			if _, ok := kinds[e.Kind]; !ok {
				continue
			}
		}
		next := e.Tgt
		if !forward {
			next = e.Src
		}
		out = append(out, predecessor{node: next, edge: e})
	}
	return out
}

// expand advances one BFS frontier by a single level, recording shortest-path
// predecessors (including ties, for multi-path) and honoring the hub and
// visited bounds.
func expand(g *domain.Graph, h *half, kinds map[domain.EdgeKind]struct{}, forward bool, opts Options, visited *int, res *Result) {
	next := make([]domain.NodeID, 0, len(h.front))
	for _, u := range h.front {
		nbrs := neighbors(g, u, kinds, forward)
		if opts.HubDegree > 0 && len(nbrs) > opts.HubDegree {
			// Do not expand THROUGH a hub: its fan-out would swamp the search.
			res.Truncated = true
			if res.Bound == "" {
				res.Bound = "hub"
			}
			continue
		}
		for _, nb := range nbrs {
			if _, seen := h.dist[nb.node]; !seen {
				if opts.MaxVisited > 0 && *visited >= opts.MaxVisited {
					res.Truncated = true
					res.Bound = "visited"
					h.front = next
					h.level++
					return
				}
				h.dist[nb.node] = h.level + 1
				*visited++
				h.preds[nb.node] = []predecessor{{node: u, edge: nb.edge}}
				next = append(next, nb.node)
			} else if h.dist[nb.node] == h.level+1 {
				// Another equally-short route to nb: keep it for multi-path.
				h.preds[nb.node] = append(h.preds[nb.node], predecessor{node: u, edge: nb.edge})
			}
		}
	}
	h.front = next
	h.level++
}

// bestMeet returns the smallest fdist+bdist over nodes reached by both halves,
// or -1 if they have not met. Scanning the smaller dist map keeps it cheap.
func bestMeet(fwd, bwd half) int {
	small, large := fwd.dist, bwd.dist
	if len(large) < len(small) {
		small, large = large, small
	}
	best := -1
	for n, d := range small {
		if d2, ok := large[n]; ok {
			if total := d + d2; best < 0 || total < best {
				best = total
			}
		}
	}
	return best
}

// reconstruct stitches forward partial paths (from -> meet) with backward
// partial paths (meet -> to) for every meet node at total distance mu, up to
// limit paths. Paths are deduped by their node+edge identity.
func reconstruct(fwd, bwd half, from, to domain.NodeID, mu, limit int) []Path {
	var out []Path
	seen := map[string]struct{}{}
	for meet, fd := range fwd.dist {
		bd, ok := bwd.dist[meet]
		if !ok || fd+bd != mu {
			continue
		}
		fwdPaths := walk(fwd.preds, meet, from, limit, true)
		bwdPaths := walk(bwd.preds, meet, to, limit, false)
		for _, fp := range fwdPaths {
			for _, bp := range bwdPaths {
				p := join(fp, bp)
				key := pathKey(p)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, p)
				if len(out) >= limit {
					return out
				}
			}
		}
	}
	return out
}

// partial is an ordered run of nodes with the edges between them, oriented from
// the search origin toward the meet node.
type partial struct {
	nodes []domain.NodeID
	edges []*domain.Edge
}

// walk enumerates up to limit shortest partial paths from `meet` back to
// `origin` over preds, returning them oriented origin->meet (forward side) or
// meet->origin (backward side) per orientFromOrigin.
func walk(preds map[domain.NodeID][]predecessor, meet, origin domain.NodeID, limit int, orientFromOrigin bool) []partial {
	var results []partial
	var dfs func(cur domain.NodeID, nodes []domain.NodeID, edges []*domain.Edge)
	dfs = func(cur domain.NodeID, nodes []domain.NodeID, edges []*domain.Edge) {
		if len(results) >= limit {
			return
		}
		if cur == origin {
			// nodes/edges were collected meet->origin; orient as requested.
			n := append([]domain.NodeID{}, nodes...)
			e := append([]*domain.Edge{}, edges...)
			if orientFromOrigin {
				reverseNodes(n)
				reverseEdges(e)
			}
			results = append(results, partial{nodes: n, edges: e})
			return
		}
		for _, p := range preds[cur] {
			// Copy per branch: a shared append backing-array would let sibling
			// recursion paths corrupt each other.
			nn := append(append([]domain.NodeID{}, nodes...), p.node)
			ee := append(append([]*domain.Edge{}, edges...), p.edge)
			dfs(p.node, nn, ee)
			if len(results) >= limit {
				return
			}
		}
	}
	dfs(meet, []domain.NodeID{meet}, nil)
	return results
}

// join concatenates a forward partial (from->meet) with a backward partial
// (meet->to), sharing the meet node once.
func join(fp, bp partial) Path {
	nodes := append([]domain.NodeID{}, fp.nodes...)
	nodes = append(nodes, bp.nodes[1:]...) // skip the shared meet node
	edges := append([]*domain.Edge{}, fp.edges...)
	edges = append(edges, bp.edges...)
	return Path{Nodes: nodes, Edges: edges}
}

func pathKey(p Path) string {
	b := make([]byte, 0, len(p.Nodes)*8)
	for _, n := range p.Nodes {
		b = append(b, []byte(n)...)
		b = append(b, 0)
	}
	return string(b)
}

func reverseNodes(s []domain.NodeID) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func reverseEdges(s []*domain.Edge) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
