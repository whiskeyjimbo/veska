// SPDX-License-Identifier: AGPL-3.0-only

package cycles_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/cycles"
)

// sccsOfSize2Plus returns the >=2-member components as sorted member slices,
// themselves sorted, so assertions are order-independent.
func sccsOfSize2Plus(edges []cycles.Edge) [][]string {
	var out [][]string
	for _, scc := range cycles.StronglyConnected(edges) {
		if len(scc) >= 2 {
			out = append(out, scc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func TestStronglyConnected_DetectsCycle(t *testing.T) {
	edges := []cycles.Edge{{Src: "a", Dst: "b"}, {Src: "b", Dst: "c"}, {Src: "c", Dst: "a"}}
	got := sccsOfSize2Plus(edges)
	want := [][]string{{"a", "b", "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStronglyConnected_NoCycle(t *testing.T) {
	edges := []cycles.Edge{{Src: "a", Dst: "b"}, {Src: "b", Dst: "c"}}
	if got := sccsOfSize2Plus(edges); len(got) != 0 {
		t.Errorf("acyclic DAG produced cycles: %v", got)
	}
}

func TestStronglyConnected_SelfLoopExcluded(t *testing.T) {
	edges := []cycles.Edge{{Src: "a", Dst: "a"}}
	if got := sccsOfSize2Plus(edges); len(got) != 0 {
		t.Errorf("self-loop should not be a >=2 cycle, got %v", got)
	}
}

func TestStronglyConnected_TwoDisjointCycles(t *testing.T) {
	edges := []cycles.Edge{
		{Src: "a", Dst: "b"}, {Src: "b", Dst: "a"},
		{Src: "x", Dst: "y"}, {Src: "y", Dst: "x"},
		{Src: "b", Dst: "x"}, // one-way bridge: does not merge the two cycles
	}
	got := sccsOfSize2Plus(edges)
	want := [][]string{{"a", "b"}, {"x", "y"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
