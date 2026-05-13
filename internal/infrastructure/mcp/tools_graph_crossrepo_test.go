package mcp

import (
	"context"
	"testing"

	application "github.com/whiskeyjimbo/engram/solov2/internal/application"
	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
	"github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/sqlite/resolver"
)

// ---------------------------------------------------------------------------
// eng_get_call_chain — cross-repo edges injected into response
// ---------------------------------------------------------------------------

// TestGetCallChainCrossRepoEdges verifies that when a ResolveFunc is provided
// and returns a ResolvedEdge for a visited node, the GraphResponse contains a
// matching CrossRepoEdge with cross_repo=true.
func TestGetCallChainCrossRepoEdges(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	// Resolver returns a cross-repo edge for node "b".
	mockResolve := func(_ context.Context, nodeID, _ string, _ bool) ([]resolver.ResolvedEdge, error) {
		if nodeID == "b" {
			return []resolver.ResolvedEdge{{
				SrcNodeID: "b",
				DstNodeID: "ext-node-1",
				DstRepoID: "other-repo",
				DstBranch: "main",
				Kind:      "calls",
				CrossRepo: true,
			}}, nil
		}
		return nil, nil
	}

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(), mockResolve)

	resp, rpcErr := dispatchGraph(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   3,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

	if len(resp.CrossRepoEdges) != 1 {
		t.Fatalf("expected 1 cross-repo edge, got %d", len(resp.CrossRepoEdges))
	}
	cre := resp.CrossRepoEdges[0]
	if !cre.CrossRepo {
		t.Error("expected CrossRepoEdge.CrossRepo to be true")
	}
	if cre.SrcNodeID != "b" {
		t.Errorf("expected SrcNodeID=b, got %q", cre.SrcNodeID)
	}
	if cre.DstNodeID != "ext-node-1" {
		t.Errorf("expected DstNodeID=ext-node-1, got %q", cre.DstNodeID)
	}
	if cre.DstRepoID != "other-repo" {
		t.Errorf("expected DstRepoID=other-repo, got %q", cre.DstRepoID)
	}
	if cre.DstBranch != "main" {
		t.Errorf("expected DstBranch=main, got %q", cre.DstBranch)
	}
	if cre.Kind != "calls" {
		t.Errorf("expected Kind=calls, got %q", cre.Kind)
	}
}

// TestGetCallChainNoCrossRepoByDefault verifies that when no ResolveFunc is
// provided (nil), the response still succeeds and contains no cross-repo edges.
func TestGetCallChainNoCrossRepoByDefault(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(), nil)

	resp, rpcErr := dispatchGraph(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   3,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

	if len(resp.CrossRepoEdges) != 0 {
		t.Errorf("expected 0 cross-repo edges with nil resolver, got %d", len(resp.CrossRepoEdges))
	}
	// In-repo traversal still works.
	if len(resp.Nodes) == 0 {
		t.Error("expected in-repo nodes to be returned even with nil resolver")
	}
}

// TestGetCallChainCrossRepoSilentMiss verifies that when the resolver returns
// nothing for a node, the response succeeds with no cross-repo edges (silent miss).
func TestGetCallChainCrossRepoSilentMiss(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	// Resolver returns nothing (silent miss).
	mockResolve := func(_ context.Context, _ string, _ string, _ bool) ([]resolver.ResolvedEdge, error) {
		return nil, nil
	}

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(), mockResolve)

	resp, rpcErr := dispatchGraph(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   3,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

	if len(resp.CrossRepoEdges) != 0 {
		t.Errorf("expected 0 cross-repo edges on silent miss, got %d", len(resp.CrossRepoEdges))
	}
}

// TestGetCallChainBFSDoesNotFollowCrossRepoEdges verifies that cross-repo edges
// do not cause BFS to continue traversal into the foreign repo. Only in-repo
// nodes appear in Nodes; cross-repo edges only appear in CrossRepoEdges.
func TestGetCallChainBFSDoesNotFollowCrossRepoEdges(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	mockResolve := func(_ context.Context, nodeID, _ string, _ bool) ([]resolver.ResolvedEdge, error) {
		if nodeID == "b" {
			return []resolver.ResolvedEdge{{
				SrcNodeID: "b",
				DstNodeID: "ext-node-x",
				DstRepoID: "external-repo",
				DstBranch: "main",
				Kind:      "calls",
				CrossRepo: true,
			}}, nil
		}
		return nil, nil
	}

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(), mockResolve)

	resp, rpcErr := dispatchGraph(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   5,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

	// In-repo: should see node b but NOT ext-node-x in Nodes.
	nodeIDs := make(map[string]bool)
	for _, n := range resp.Nodes {
		nodeIDs[string(n.ID)] = true
	}
	if nodeIDs["ext-node-x"] {
		t.Error("BFS must not follow cross-repo edges; ext-node-x should not be in Nodes")
	}
	if !nodeIDs["b"] {
		t.Error("expected in-repo node b to appear in Nodes")
	}

	// Cross-repo edges are in their own collection.
	if len(resp.CrossRepoEdges) != 1 {
		t.Fatalf("expected 1 cross-repo edge, got %d", len(resp.CrossRepoEdges))
	}
}
