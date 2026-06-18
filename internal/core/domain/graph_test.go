// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

import (
	"testing"
)

// helpers - build minimal valid nodes and edges

func makeNode(t *testing.T, id string) *Node {
	t.Helper()
	n, err := NewNode(NodeSpec{ID: id, Path: "pkg/foo.go", Name: id, Kind: KindFunction})
	if err != nil {
		t.Fatalf("makeNode(%q): %v", id, err)
	}
	return n
}

func makeEdge(t *testing.T, src, tgt NodeID) *Edge {
	t.Helper()
	e, err := NewEdge(EdgeSpec{Src: src, Tgt: tgt, Kind: EdgeCalls})
	if err != nil {
		t.Fatalf("makeEdge(%q->%q): %v", src, tgt, err)
	}
	return e
}

// ── NewGraph ──────────────────────────────────────────────────────────────────

func TestNewGraph_ValidArgs(t *testing.T) {
	g, err := NewGraph("repo-1", "main")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if g == nil {
		t.Fatal("expected non-nil Graph")
		return
	}
}

func TestNewGraph_EmptyRepoID(t *testing.T) {
	_, err := NewGraph("", "main")
	if err == nil {
		t.Fatal("expected error for empty repoID")
		return
	}
}

func TestNewGraph_EmptyBranch(t *testing.T) {
	_, err := NewGraph("repo-1", "")
	if err == nil {
		t.Fatal("expected error for empty branch")
		return
	}
}

func TestNewGraph_ScopedFields(t *testing.T) {
	g, _ := NewGraph("my-repo", "feature/x")
	if g.RepoID != "my-repo" {
		t.Errorf("RepoID = %q, want %q", g.RepoID, "my-repo")
	}
	if g.Branch != "feature/x" {
		t.Errorf("Branch = %q, want %q", g.Branch, "feature/x")
	}
}

// ── AddNode / Node ────────────────────────────────────────────────────────────

func TestGraph_AddNode_And_Lookup(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	n := makeNode(t, "node-1")

	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	got, ok := g.Node(NodeID("node-1"))
	if !ok {
		t.Fatal("Node lookup returned false, want true")
	}
	if got != n {
		t.Fatal("returned node pointer mismatch")
	}
}

func TestGraph_AddNode_DuplicateID(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	n := makeNode(t, "dup")
	if err := g.AddNode(n); err != nil {
		t.Fatalf("first AddNode: %v", err)
	}
	n2 := makeNode(t, "dup")
	if err := g.AddNode(n2); err == nil {
		t.Fatal("expected error for duplicate node ID")
		return
	}
}

func TestGraph_AddNode_Nil(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	if err := g.AddNode(nil); err == nil {
		t.Fatal("expected error for nil node, got nil")
	}
}

func TestGraph_AddEdge_Nil(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	if err := g.AddEdge(nil); err == nil {
		t.Fatal("expected error for nil edge, got nil")
	}
}

func TestGraph_Node_NotFound(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	got, ok := g.Node(NodeID("missing"))
	if ok {
		t.Fatal("expected ok=false for missing node")
	}
	if got != nil {
		t.Fatal("expected nil node for missing ID")
	}
}

// ── AddEdge ───────────────────────────────────────────────────────────────────

func TestGraph_AddEdge_Valid(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	a := makeNode(t, "a")
	b := makeNode(t, "b")
	_ = g.AddNode(a)
	_ = g.AddNode(b)

	e := makeEdge(t, "a", "b")
	if err := g.AddEdge(e); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
}

func TestGraph_AddEdge_MissingSrc(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	b := makeNode(t, "b")
	_ = g.AddNode(b)

	e := makeEdge(t, "missing-src", "b")
	if err := g.AddEdge(e); err == nil {
		t.Fatal("expected error when src node not in graph")
		return
	}
}

func TestGraph_AddEdge_MissingTgt(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	a := makeNode(t, "a")
	_ = g.AddNode(a)

	e := makeEdge(t, "a", "missing-tgt")
	if err := g.AddEdge(e); err == nil {
		t.Fatal("expected error when tgt node not in graph")
		return
	}
}

func TestGraph_AddEdge_BothEndpointsMissing(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	e := makeEdge(t, "x", "y")
	if err := g.AddEdge(e); err == nil {
		t.Fatal("expected error when both endpoints missing")
		return
	}
}

// ── OutgoingEdges ─────────────────────────────────────────────────────────────

func TestGraph_OutgoingEdges_NonEmpty(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	a, b, c := makeNode(t, "a"), makeNode(t, "b"), makeNode(t, "c")
	_ = g.AddNode(a)
	_ = g.AddNode(b)
	_ = g.AddNode(c)

	e1, e2 := makeEdge(t, "a", "b"), makeEdge(t, "a", "c")
	_ = g.AddEdge(e1)
	_ = g.AddEdge(e2)

	out := g.OutgoingEdges(NodeID("a"))
	if len(out) != 2 {
		t.Fatalf("expected 2 outgoing edges, got %d", len(out))
	}
}

func TestGraph_OutgoingEdges_EmptySliceNotNil(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	n := makeNode(t, "lonely")
	_ = g.AddNode(n)

	out := g.OutgoingEdges(NodeID("lonely"))
	if out == nil {
		t.Fatal("OutgoingEdges must return empty slice, not nil")
		return
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 outgoing edges, got %d", len(out))
	}
}

func TestGraph_OutgoingEdges_NodeNotInGraph(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	out := g.OutgoingEdges(NodeID("ghost"))
	if out == nil {
		t.Fatal("OutgoingEdges must return empty slice, not nil")
		return
	}
}

// ── IncomingEdges ─────────────────────────────────────────────────────────────

func TestGraph_IncomingEdges_NonEmpty(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	a, b, c := makeNode(t, "a"), makeNode(t, "b"), makeNode(t, "c")
	_ = g.AddNode(a)
	_ = g.AddNode(b)
	_ = g.AddNode(c)

	e1, e2 := makeEdge(t, "b", "a"), makeEdge(t, "c", "a")
	_ = g.AddEdge(e1)
	_ = g.AddEdge(e2)

	in := g.IncomingEdges(NodeID("a"))
	if len(in) != 2 {
		t.Fatalf("expected 2 incoming edges, got %d", len(in))
	}
}

func TestGraph_IncomingEdges_EmptySliceNotNil(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	n := makeNode(t, "root")
	_ = g.AddNode(n)

	in := g.IncomingEdges(NodeID("root"))
	if in == nil {
		t.Fatal("IncomingEdges must return empty slice, not nil")
		return
	}
	if len(in) != 0 {
		t.Fatalf("expected 0 incoming edges, got %d", len(in))
	}
}

func TestGraph_IncomingEdges_NodeNotInGraph(t *testing.T) {
	g, _ := NewGraph("repo", "main")
	in := g.IncomingEdges(NodeID("ghost"))
	if in == nil {
		t.Fatal("IncomingEdges must return empty slice, not nil")
		return
	}
}
