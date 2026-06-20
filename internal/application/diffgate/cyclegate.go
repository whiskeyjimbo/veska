// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import "sort"

// DependencyKinds are the edge kinds whose cycles this gate detects. Today only
// CALLS edges are emitted (by the Go parser); IMPORTS is included so that when a
// language whose parser emits module/file IMPORTS edges is added - Python, JS,
// Ruby permit circular imports where Go's compiler forbids them - the same gate
// flags those net-new cycles with no change here. A kind absent from the graph
// simply matches nothing.
var DependencyKinds = []string{"CALLS", "IMPORTS"}

// FailNewCycle names the failing check: the candidate introduced a dependency
// cycle (a strongly-connected component of >=2 symbols) absent at base.
const FailNewCycle = "new_cycle"

// DirectedEdge is one src->dst dependency edge in the after- or base-state graph.
type DirectedEdge struct{ Src, Dst string }

// CycleMember is one symbol participating in a net-new dependency cycle.
type CycleMember struct {
	NodeID     string `json:"node_id"`
	FilePath   string `json:"file_path"`
	SymbolPath string `json:"symbol_path"`
}

// CycleGroup is the member set of one net-new strongly-connected component.
type CycleGroup struct {
	Members []CycleMember `json:"members"`
}

// CycleVerdict is the cycle gate's pass/fail result. Unlike the clone gate this
// gate has no degraded "unchecked" mode: the after- and base-state dependency
// graphs are always materializable from the (cloned) base graph, so
// Pass == (len(NewCycles) == 0).
type CycleVerdict struct {
	Pass      bool         `json:"pass"`
	NewCycles []CycleGroup `json:"new_cycles"`
}

// Failures returns the stable failing-check name for CI/agent consumption.
func (v CycleVerdict) Failures() []string {
	if v.Pass {
		return nil
	}
	return []string{FailNewCycle}
}

// ExitCode is the process exit code for CI gating: 0 on PASS, 1 on FAIL.
func (v CycleVerdict) ExitCode() int {
	if v.Pass {
		return 0
	}
	return 1
}

// CycleGate flags a candidate change that introduces a net-new dependency cycle:
// a strongly-connected component (SCC) of >=2 symbols, mutually reachable over
// DependencyKinds edges, whose members were NOT already a single cycle at base.
// Self-loops (direct recursion A->A) are excluded - they are size-1 SCCs and
// ubiquitous/benign.
// Net-new is decided per after-state SCC C (|C|>=2): C is flagged iff its members
// are NOT all contained in one base SCC - i.e. they were not already mutually
// reachable before the change. A symbol added by the change has no base SCC, so a
// cycle formed with newly-added code trivially fails containment and is caught.
// Flagged SCCs are further scoped to those touching the change's node set, so a
// pre-existing cycle elsewhere can never surface from an unrelated graph-build
// difference (defensive, consistent with the coverage gate's intersection).
// Language-agnostic: the algorithm is pure graph topology over DependencyKinds
// and makes no Go-specific assumption. On compiling Go the only catchable cycle
// is within-package mutual recursion (the compiler forbids package import
// cycles); other languages permit import cycles the same gate then catches.
type CycleGate struct{}

// NewCycleGate constructs a CycleGate. It is stateless.
func NewCycleGate() *CycleGate { return &CycleGate{} }

// Evaluate flags net-new cycles. afterEdges/baseEdges are the after- and
// base-state directed dependency graphs; changedNodeIDs is the node-precision
// change set; info names a node_id for the verdict (a missing id falls back to
// the bare id). Pure - no I/O.
func (g *CycleGate) Evaluate(afterEdges, baseEdges []DirectedEdge, changedNodeIDs []string, info map[string]CycleMember) CycleVerdict {
	baseComp := componentIndex(stronglyConnected(baseEdges))
	changed := make(map[string]struct{}, len(changedNodeIDs))
	for _, id := range changedNodeIDs {
		changed[id] = struct{}{}
	}

	var groups []CycleGroup
	for _, scc := range stronglyConnected(afterEdges) {
		if len(scc) < 2 {
			continue // not a cycle (self-loops are size-1 and excluded)
		}
		if containedInOneBaseSCC(scc, baseComp) {
			continue // the cycle already existed at base
		}
		if !touchesChanged(scc, changed) {
			continue // not introduced by this change; defensive scoping
		}
		groups = append(groups, CycleGroup{Members: members(scc, info)})
	}
	sort.Slice(groups, func(i, j int) bool { return firstID(groups[i]) < firstID(groups[j]) })
	return CycleVerdict{Pass: len(groups) == 0, NewCycles: groups}
}

// containedInOneBaseSCC reports whether every member of scc belonged to the SAME
// base SCC (of size>=2). If so the members were already mutually reachable, so
// the cycle is pre-existing. A member absent from any base >=2 component (e.g. a
// newly-added symbol) makes containment false.
func containedInOneBaseSCC(scc []string, baseComp map[string]int) bool {
	first, ok := baseComp[scc[0]]
	if !ok {
		return false
	}
	for _, id := range scc[1:] {
		if c, ok := baseComp[id]; !ok || c != first {
			return false
		}
	}
	return true
}

func touchesChanged(scc []string, changed map[string]struct{}) bool {
	for _, id := range scc {
		if _, ok := changed[id]; ok {
			return true
		}
	}
	return false
}

// componentIndex maps each node in a >=2-sized SCC to a stable component id.
// Nodes in singleton SCCs are omitted - they are not cycles.
func componentIndex(sccs [][]string) map[string]int {
	idx := make(map[string]int)
	for i, scc := range sccs {
		if len(scc) < 2 {
			continue
		}
		for _, id := range scc {
			idx[id] = i
		}
	}
	return idx
}

func members(scc []string, info map[string]CycleMember) []CycleMember {
	out := make([]CycleMember, 0, len(scc))
	for _, id := range scc {
		if m, ok := info[id]; ok {
			out = append(out, m)
			continue
		}
		out = append(out, CycleMember{NodeID: id})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

func firstID(g CycleGroup) string {
	if len(g.Members) == 0 {
		return ""
	}
	return g.Members[0].NodeID
}

// stronglyConnected returns the strongly-connected components of the directed
// graph described by edges, via an ITERATIVE Tarjan (iterative to avoid stack
// overflow on a large code graph). Self-loops are dropped: they would make a
// node its own size-1 SCC, which callers discard anyway. Iteration is sorted
// (nodes and each adjacency list) for deterministic component output.
func stronglyConnected(edges []DirectedEdge) [][]string {
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
