// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"reflect"
	"sort"
	"testing"
)

func edges(pairs ...[2]string) []DirectedEdge {
	out := make([]DirectedEdge, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, DirectedEdge{Src: p[0], Dst: p[1]})
	}
	return out
}

// memberIDs flattens a verdict's cycle members to a sorted id slice for asserts.
func memberIDs(v CycleVerdict) []string {
	var ids []string
	for _, g := range v.NewCycles {
		for _, m := range g.Members {
			ids = append(ids, m.NodeID)
		}
	}
	sort.Strings(ids)
	return ids
}

// AC1: a change that forms a cycle absent at base FAILs and names the members.
func TestCycleGate_NewCycle_Fails(t *testing.T) {
	base := edges([2]string{"A", "B"})                       // A->B, acyclic
	after := edges([2]string{"A", "B"}, [2]string{"B", "A"}) // B->A added -> A<->B
	v := NewCycleGate().Evaluate(after, base, []string{"B"}, nil)
	if v.Pass {
		t.Fatalf("new A<->B cycle must FAIL; got %+v", v)
	}
	if got := memberIDs(v); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("members = %v, want [A B]", got)
	}
	if !reflect.DeepEqual(v.Failures(), []string{FailNewCycle}) {
		t.Fatalf("failures = %v", v.Failures())
	}
}

// AC2: adding an edge that forms NO cycle passes.
func TestCycleGate_AcyclicAdd_Passes(t *testing.T) {
	base := edges()                     // A,B isolated
	after := edges([2]string{"A", "B"}) // A->B only
	v := NewCycleGate().Evaluate(after, base, []string{"A"}, nil)
	if !v.Pass {
		t.Fatalf("acyclic edge-add must PASS; got %+v", v)
	}
}

// A cycle that already existed at base is not net-new -> PASS, even though the
// change touched a member.
func TestCycleGate_PreExistingCycle_Passes(t *testing.T) {
	base := edges([2]string{"A", "B"}, [2]string{"B", "A"})
	after := edges([2]string{"A", "B"}, [2]string{"B", "A"})
	v := NewCycleGate().Evaluate(after, base, []string{"A"}, nil)
	if !v.Pass {
		t.Fatalf("pre-existing cycle must PASS; got %+v", v)
	}
}

// Growing a pre-existing 2-cycle into a 3-cycle is net-new: the {A,B,C} SCC is
// not contained in the base {A,B} SCC -> FAIL.
func TestCycleGate_GrowCycle_Fails(t *testing.T) {
	base := edges([2]string{"A", "B"}, [2]string{"B", "A"})
	after := edges([2]string{"A", "B"}, [2]string{"B", "C"}, [2]string{"C", "A"})
	v := NewCycleGate().Evaluate(after, base, []string{"C"}, nil)
	if v.Pass {
		t.Fatalf("growing 2-cycle to 3-cycle must FAIL; got %+v", v)
	}
	if got := memberIDs(v); !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatalf("members = %v, want [A B C]", got)
	}
}

// Self-loop (direct recursion A->A) is not a >=2 cycle -> PASS.
func TestCycleGate_SelfLoop_Passes(t *testing.T) {
	base := edges()
	after := edges([2]string{"A", "A"})
	v := NewCycleGate().Evaluate(after, base, []string{"A"}, nil)
	if !v.Pass {
		t.Fatalf("self-loop must PASS (excluded); got %+v", v)
	}
}

// Defensive scoping: a net-new cycle that does NOT touch the change set is not
// flagged (it cannot have been introduced by this change). Guards against an
// unrelated graph-build difference surfacing a spurious cycle.
func TestCycleGate_NewCycleOutsideChangeSet_Passes(t *testing.T) {
	base := edges([2]string{"A", "B"})
	after := edges([2]string{"A", "B"}, [2]string{"B", "A"})
	v := NewCycleGate().Evaluate(after, base, []string{"Z"}, nil) // Z unrelated
	if !v.Pass {
		t.Fatalf("cycle not touching the change set must PASS; got %+v", v)
	}
}

// A cycle built entirely from newly-added symbols (no base SCC) fails
// containment and is flagged.
func TestCycleGate_NewSymbolsCycle_Fails(t *testing.T) {
	base := edges()
	after := edges([2]string{"X", "Y"}, [2]string{"Y", "X"})
	v := NewCycleGate().Evaluate(after, base, []string{"X", "Y"}, nil)
	if v.Pass {
		t.Fatalf("cycle of new symbols must FAIL; got %+v", v)
	}
}

// stronglyConnected sanity: two disjoint cycles + an acyclic tail.
func TestStronglyConnected_DisjointComponents(t *testing.T) {
	got := stronglyConnected(edges(
		[2]string{"A", "B"}, [2]string{"B", "A"}, // SCC {A,B}
		[2]string{"C", "D"}, [2]string{"D", "C"}, // SCC {C,D}
		[2]string{"B", "C"}, // bridge (no merge: one-directional)
		[2]string{"E", "A"}, // acyclic source
	))
	var multi [][]string
	for _, scc := range got {
		if len(scc) >= 2 {
			multi = append(multi, scc)
		}
	}
	sort.Slice(multi, func(i, j int) bool { return multi[i][0] < multi[j][0] })
	want := [][]string{{"A", "B"}, {"C", "D"}}
	if !reflect.DeepEqual(multi, want) {
		t.Fatalf("SCCs = %v, want %v", multi, want)
	}
}
