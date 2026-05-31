package mcp

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// solov2-2n33 verification — eng_get_call_chain accepts direction =
// out (default), in, or both, and walks the requested edge directions.
// Uses the same in-package fixture helpers (newStubGraphStorage, mustNode,
// mustEdge, dispatchCallChain) that the existing TestGetCallChain_*
// suite uses, so the new direction tests stay aligned with the others.

func TestGetCallChain_DirectionIn_WalksIncomingEdges(t *testing.T) {
	store := newStubGraphStorage()
	// Build: A -> B. Querying B with direction=in must surface A as a caller.
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id":   "b",
		"repo_id":   "repo1",
		"branch":    "main",
		"direction": "in",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	foundA := false
	for _, n := range resp.Nodes {
		if n.NodeID == "a" {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("direction=in must surface incoming caller A; got %+v", resp.Nodes)
	}
}

func TestGetCallChain_DirectionOutIsDefault(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	// No direction → default 'out'. From A, must reach B.
	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	foundB := false
	for _, n := range resp.Nodes {
		if n.NodeID == "b" {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("default direction must walk outgoing; expected B in %+v", resp.Nodes)
	}
}

func TestGetCallChain_DirectionBoth_WalksEitherWay(t *testing.T) {
	store := newStubGraphStorage()
	// Build: A -> B -> C. From B with direction=both we should reach BOTH A
	// (incoming caller) and C (outgoing callee).
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	c := mustNode(t, "c", "pkg/c.go", "C", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addNode(c)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))
	store.addEdge(mustEdge(t, "b", "c", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id":   "b",
		"repo_id":   "repo1",
		"branch":    "main",
		"direction": "both",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	seen := map[string]bool{}
	for _, n := range resp.Nodes {
		seen[n.NodeID] = true
	}
	if !seen["a"] {
		t.Errorf("direction=both must include incoming caller A; got %+v", resp.Nodes)
	}
	if !seen["c"] {
		t.Errorf("direction=both must include outgoing callee C; got %+v", resp.Nodes)
	}
}

func TestGetCallChain_RejectsInvalidDirection(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	store.addNode(a)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	_, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id":   "a",
		"repo_id":   "repo1",
		"branch":    "main",
		"direction": "sideways",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("invalid direction must return CodeInvalidParams, got %+v", rpcErr)
	}
}
