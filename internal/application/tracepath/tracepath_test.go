// SPDX-License-Identifier: AGPL-3.0-only

package tracepath_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/tracepath"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// buildGraph constructs a graph from "src>dst" edge specs of the given kind.
func buildGraph(t *testing.T, kind domain.EdgeKind, specs ...string) *domain.Graph {
	t.Helper()
	return buildMixed(t, func(string) domain.EdgeKind { return kind }, specs...)
}

// buildMixed builds a graph where each edge's kind is chosen by kindFor(spec).
func buildMixed(t *testing.T, kindFor func(string) domain.EdgeKind, specs ...string) *domain.Graph {
	t.Helper()
	g, err := domain.NewGraph("r1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	seen := map[string]bool{}
	addNode := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: id + ".go", Name: id, Kind: domain.KindFunction})
		if err != nil {
			t.Fatalf("NewNode %s: %v", id, err)
		}
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
	}
	for _, s := range specs {
		parts := strings.SplitN(s, ">", 2)
		src, dst := parts[0], parts[1]
		addNode(src)
		addNode(dst)
		e, err := domain.NewEdge(domain.EdgeSpec{Src: domain.NodeID(src), Tgt: domain.NodeID(dst), Kind: kindFor(s)},
			domain.WithConfidence(domain.Definite))
		if err != nil {
			t.Fatalf("NewEdge %s: %v", s, err)
		}
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge %s: %v", s, err)
		}
	}
	return g
}

func pathString(p tracepath.Path) string {
	ids := make([]string, len(p.Nodes))
	for i, n := range p.Nodes {
		ids[i] = string(n)
	}
	return strings.Join(ids, ">")
}

func TestFind_ShortestPath(t *testing.T) {
	g := buildGraph(t, domain.EdgeCalls, "A>B", "B>C", "C>D", "A>X", "X>D")
	// A reaches D via A>X>D (len 2), shorter than A>B>C>D (len 3).
	res := tracepath.Find(g, "A", "D", tracepath.Options{MaxDepth: 10, MaxPaths: 1})
	if len(res.Paths) != 1 {
		t.Fatalf("got %d paths, want 1 (reason=%q)", len(res.Paths), res.Reason)
	}
	if got := pathString(res.Paths[0]); got != "A>X>D" {
		t.Errorf("shortest path = %q, want A>X>D", got)
	}
	if n := len(res.Paths[0].Edges); n != 2 {
		t.Errorf("edges = %d, want 2", n)
	}
}

func TestFind_NoPath(t *testing.T) {
	g := buildGraph(t, domain.EdgeCalls, "A>B", "B>C", "D>E")
	res := tracepath.Find(g, "A", "E", tracepath.Options{MaxDepth: 10, MaxPaths: 1})
	if len(res.Paths) != 0 {
		t.Fatalf("got %d paths, want 0", len(res.Paths))
	}
	if res.Reason == "" {
		t.Error("expected a no-path reason, got empty")
	}
}

func TestFind_RespectsDepthBound(t *testing.T) {
	g := buildGraph(t, domain.EdgeCalls, "A>B", "B>C", "C>D", "D>E")
	// A>B>C>D>E is length 4; a depth bound of 3 must not find it.
	res := tracepath.Find(g, "A", "E", tracepath.Options{MaxDepth: 3, MaxPaths: 1})
	if len(res.Paths) != 0 {
		t.Fatalf("depth 3 should miss the length-4 path, got %v", pathString(res.Paths[0]))
	}
	// With a sufficient depth it is found.
	res = tracepath.Find(g, "A", "E", tracepath.Options{MaxDepth: 4, MaxPaths: 1})
	if len(res.Paths) != 1 || pathString(res.Paths[0]) != "A>B>C>D>E" {
		t.Fatalf("depth 4 should find the path, got %+v", res)
	}
}

func TestFind_EdgeKindFilter(t *testing.T) {
	// A --CALLS--> Iface.Write ; Iface.Write --IMPLEMENTS--> Concrete.Write.
	// The CALLS path dead-ends at the interface method; only including
	// IMPLEMENTS bridges to the concrete implementation.
	g := buildMixed(t, func(s string) domain.EdgeKind {
		if s == "A>I" {
			return domain.EdgeCalls
		}
		return domain.EdgeImplements
	}, "A>I", "I>C")

	callsOnly := tracepath.Find(g, "A", "C", tracepath.Options{EdgeKinds: []domain.EdgeKind{domain.EdgeCalls}, MaxDepth: 10, MaxPaths: 1})
	if len(callsOnly.Paths) != 0 {
		t.Errorf("CALLS-only should not reach C through an IMPLEMENTS edge, got %v", pathString(callsOnly.Paths[0]))
	}
	withImpl := tracepath.Find(g, "A", "C", tracepath.Options{EdgeKinds: []domain.EdgeKind{domain.EdgeCalls, domain.EdgeImplements}, MaxDepth: 10, MaxPaths: 1})
	if len(withImpl.Paths) != 1 || pathString(withImpl.Paths[0]) != "A>I>C" {
		t.Errorf("CALLS+IMPLEMENTS should find A>I>C, got %+v", withImpl)
	}
}

func TestFind_MultiplePaths(t *testing.T) {
	// Two distinct shortest paths A>B>D and A>C>D.
	g := buildGraph(t, domain.EdgeCalls, "A>B", "A>C", "B>D", "C>D")
	res := tracepath.Find(g, "A", "D", tracepath.Options{MaxDepth: 10, MaxPaths: 5})
	if len(res.Paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(res.Paths))
	}
	got := []string{pathString(res.Paths[0]), pathString(res.Paths[1])}
	sort.Strings(got)
	if got[0] != "A>B>D" || got[1] != "A>C>D" {
		t.Errorf("paths = %v, want [A>B>D A>C>D]", got)
	}
}

func TestFind_MaxPathsCap(t *testing.T) {
	g := buildGraph(t, domain.EdgeCalls, "A>B", "A>C", "B>D", "C>D")
	res := tracepath.Find(g, "A", "D", tracepath.Options{MaxDepth: 10, MaxPaths: 1})
	if len(res.Paths) != 1 {
		t.Fatalf("max_paths=1 should cap to a single path, got %d", len(res.Paths))
	}
}

func TestFind_HubCapTruncates(t *testing.T) {
	// A>H, then H fans out to 5 leaves; target T is unreachable. Expanding H
	// exceeds the hub degree, so the search stops and reports the hub bound.
	g := buildGraph(t, domain.EdgeCalls, "A>H", "H>n1", "H>n2", "H>n3", "H>n4", "H>n5", "Z>T")
	res := tracepath.Find(g, "A", "T", tracepath.Options{MaxDepth: 10, MaxPaths: 1, HubDegree: 3})
	if len(res.Paths) != 0 {
		t.Fatalf("expected no path, got %v", pathString(res.Paths[0]))
	}
	if !res.Truncated || res.Bound != "hub" {
		t.Errorf("expected truncated by hub, got truncated=%v bound=%q", res.Truncated, res.Bound)
	}
}

func TestFind_VisitedCapTruncates(t *testing.T) {
	g := buildGraph(t, domain.EdgeCalls, "A>B", "B>C", "C>D", "D>E", "E>F", "Z>T")
	res := tracepath.Find(g, "A", "T", tracepath.Options{MaxDepth: 20, MaxPaths: 1, MaxVisited: 3})
	if len(res.Paths) != 0 {
		t.Fatalf("expected no path, got %v", pathString(res.Paths[0]))
	}
	if !res.Truncated || res.Bound != "visited" {
		t.Errorf("expected truncated by visited, got truncated=%v bound=%q", res.Truncated, res.Bound)
	}
}

func TestFind_SourceEqualsTarget(t *testing.T) {
	g := buildGraph(t, domain.EdgeCalls, "A>B")
	res := tracepath.Find(g, "A", "A", tracepath.Options{MaxDepth: 10, MaxPaths: 1})
	if len(res.Paths) != 1 || len(res.Paths[0].Nodes) != 1 || res.Paths[0].Nodes[0] != "A" {
		t.Fatalf("from==to should yield a single trivial path [A], got %+v", res)
	}
}
