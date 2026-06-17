package mcp

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/resolver"
)

// We verify that when a cross-repository resolver function is configured, the resulting call chain response successfully maps external edges.
func TestGetCallChainCrossRepoEdges(t *testing.T) {
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
	RegisterGraphTools(r, store, staging.NewArea(), WithResolveFunc(mockResolve))

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
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

// If no resolver function is provided, the call chain succeeds but omits the cross_repo_edges field.
func TestGetCallChainNoCrossRepoByDefault(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
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
	if len(resp.Nodes) == 0 {
		t.Error("expected in-repo nodes to be returned even with nil resolver")
	}
}

// If the resolver returns an empty set of external edges, the request succeeds with an empty cross-repo edge collection.
func TestGetCallChainCrossRepoSilentMiss(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	mockResolve := func(_ context.Context, _ string, _ string, _ bool) ([]resolver.ResolvedEdge, error) {
		return nil, nil
	}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithResolveFunc(mockResolve))

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
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

// The graph search traversal must not traverse across external dependency boundaries into other repositories.
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
	RegisterGraphTools(r, store, staging.NewArea(), WithResolveFunc(mockResolve))

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   5,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

	nodeIDs := make(map[string]bool)
	for _, n := range resp.Nodes {
		nodeIDs[n.NodeID] = true
	}
	if nodeIDs["ext-node-x"] {
		t.Error("BFS must not follow cross-repo edges; ext-node-x should not be in Nodes")
	}
	if !nodeIDs["b"] {
		t.Error("expected in-repo node b to appear in Nodes")
	}

	if len(resp.CrossRepoEdges) != 1 {
		t.Fatalf("expected 1 cross-repo edge, got %d", len(resp.CrossRepoEdges))
	}
}
