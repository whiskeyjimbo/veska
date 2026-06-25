// SPDX-License-Identifier: AGPL-3.0-only

// Package cycles provides strongly-connected-component detection over a directed
// graph, shared by dependency-cycle consumers: the repo-wide import-cycle check
// and the diff-scoped cycle gate. The algorithm is pure graph topology, so it is
// language- and domain-agnostic - callers supply edges as opaque string node IDs
// (package dirs, symbol IDs, etc.).
package cycles

import "sort"

// Edge is one directed src->dst dependency edge.
type Edge struct{ Src, Dst string }

// StronglyConnected returns the strongly-connected components of the graph
// induced by edges, via an iterative Tarjan (explicit stack, so a deep graph
// cannot overflow the goroutine stack). Self-loops are dropped: a node would
// otherwise be its own size-1 SCC, which cycle callers discard anyway. Nodes and
// each adjacency list are iterated in sorted order so component output is
// deterministic. A component of size >= 2 is a dependency cycle.
func StronglyConnected(edges []Edge) [][]string {
	adj := make(map[string][]string)
	nodeset := make(map[string]struct{})
	for _, e := range edges {
		nodeset[e.Src] = struct{}{}
		nodeset[e.Dst] = struct{}{}
		if e.Src == e.Dst {
			continue // self-loop: not a >=2 cycle
		}
		adj[e.Src] = append(adj[e.Src], e.Dst)
	}
	nodes := make([]string, 0, len(nodeset))
	for n := range nodeset {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for n := range adj {
		sort.Strings(adj[n])
	}

	index := make(map[string]int)
	lowlink := make(map[string]int)
	onStack := make(map[string]bool)
	var stack []string
	next := 0
	var sccs [][]string

	type frame struct {
		node string
		ei   int // next adjacency index to process
	}

	push := func(v string) {
		index[v] = next
		lowlink[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true
	}

	for _, root := range nodes {
		if _, seen := index[root]; seen {
			continue
		}
		push(root)
		call := []frame{{node: root}}

		for len(call) > 0 {
			f := &call[len(call)-1]
			v := f.node
			if f.ei < len(adj[v]) {
				w := adj[v][f.ei]
				f.ei++
				if _, seen := index[w]; !seen {
					push(w)
					call = append(call, frame{node: w})
				} else if onStack[w] && index[w] < lowlink[v] {
					lowlink[v] = index[w]
				}
				continue
			}
			// v fully explored: if it roots an SCC, pop the component.
			if lowlink[v] == index[v] {
				var comp []string
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp = append(comp, w)
					if w == v {
						break
					}
				}
				sort.Strings(comp)
				sccs = append(sccs, comp)
			}
			call = call[:len(call)-1]
			if len(call) > 0 { // propagate lowlink to parent (post-"recursion")
				p := call[len(call)-1].node
				if lowlink[v] < lowlink[p] {
					lowlink[p] = lowlink[v]
				}
			}
		}
	}
	return sccs
}
