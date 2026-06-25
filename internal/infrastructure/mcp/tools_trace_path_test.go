// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func dispatchTracePath(t *testing.T, r *Registry, params any) (tracePathResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, &Request{Method: "eng_trace_path", Params: json.RawMessage(raw)})
	if rpcErr != nil {
		return tracePathResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var resp tracePathResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal tracePathResponse: %v", err)
	}
	return resp, nil
}

func TestTracePath_CallsPathEndToEnd(t *testing.T) {
	s := newStubGraphStorage()
	s.addNode(mustNode(t, "A", "a.go", "A", domain.KindFunction))
	s.addNode(mustNode(t, "B", "b.go", "B", domain.KindFunction))
	s.addNode(mustNode(t, "C", "c.go", "C", domain.KindFunction))
	s.addEdge(mustEdge(t, "A", "B", domain.EdgeCalls))
	s.addEdge(mustEdge(t, "B", "C", domain.EdgeCalls))
	r := NewRegistry()
	RegisterGraphTools(r, s, staging.NewArea())

	resp, rpcErr := dispatchTracePath(t, r, map[string]any{
		"from_node_id": "A", "to_node_id": "C", "repo_id": "r1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Paths) != 1 {
		t.Fatalf("got %d paths, want 1 (reason=%q)", len(resp.Paths), resp.Reason)
	}
	names := []string{}
	for _, n := range resp.Paths[0].Nodes {
		names = append(names, n.Name)
	}
	if len(names) != 3 || names[0] != "A" || names[1] != "B" || names[2] != "C" {
		t.Errorf("path nodes = %v, want [A B C]", names)
	}
	if len(resp.Paths[0].Edges) != 2 {
		t.Errorf("path edges = %d, want 2", len(resp.Paths[0].Edges))
	}
}

func TestTracePath_AmbiguousSymbolRejected(t *testing.T) {
	s := newStubGraphStorage()
	s.addNode(mustNode(t, "d1", "x.go", "Dup", domain.KindFunction))
	s.addNode(mustNode(t, "d2", "y.go", "Dup", domain.KindFunction))
	s.addNode(mustNode(t, "C", "c.go", "C", domain.KindFunction))
	r := NewRegistry()
	RegisterGraphTools(r, s, staging.NewArea())

	_, rpcErr := dispatchTracePath(t, r, map[string]any{
		"from_symbol": "Dup", "to_node_id": "C", "repo_id": "r1", "branch": "main",
	})
	if rpcErr == nil {
		t.Fatal("expected an ambiguity error for a duplicate symbol, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want CodeInvalidParams", rpcErr.Code)
	}
}

func TestTracePath_NoPathReturnsReasonNotError(t *testing.T) {
	s := newStubGraphStorage()
	s.addNode(mustNode(t, "A", "a.go", "A", domain.KindFunction))
	s.addNode(mustNode(t, "B", "b.go", "B", domain.KindFunction))
	s.addNode(mustNode(t, "C", "c.go", "C", domain.KindFunction))
	s.addEdge(mustEdge(t, "A", "B", domain.EdgeCalls)) // C is unreachable from A
	r := NewRegistry()
	RegisterGraphTools(r, s, staging.NewArea())

	resp, rpcErr := dispatchTracePath(t, r, map[string]any{
		"from_node_id": "A", "to_node_id": "C", "repo_id": "r1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("no-path must not be an error, got %+v", rpcErr)
	}
	if len(resp.Paths) != 0 {
		t.Fatalf("expected no paths, got %d", len(resp.Paths))
	}
	if resp.Reason == "" {
		t.Error("expected a reason for the empty result")
	}
}

func TestTracePath_EdgeKindsBridgeInterfaceDispatch(t *testing.T) {
	// A --CALLS--> I (interface method); I --IMPLEMENTS--> C (concrete). The
	// CALLS-only default dead-ends at I; including IMPLEMENTS reaches C.
	s := newStubGraphStorage()
	s.addNode(mustNode(t, "A", "a.go", "A", domain.KindFunction))
	s.addNode(mustNode(t, "I", "i.go", "I", domain.KindInterface))
	s.addNode(mustNode(t, "C", "c.go", "C", domain.KindStruct))
	s.addEdge(mustEdge(t, "A", "I", domain.EdgeCalls))
	s.addEdge(mustEdge(t, "I", "C", domain.EdgeImplements))
	r := NewRegistry()
	RegisterGraphTools(r, s, staging.NewArea())

	base := map[string]any{"from_node_id": "A", "to_node_id": "C", "repo_id": "r1", "branch": "main"}
	callsOnly, rpcErr := dispatchTracePath(t, r, base)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(callsOnly.Paths) != 0 {
		t.Errorf("CALLS-only should not reach C, got %d paths", len(callsOnly.Paths))
	}

	withImpl := map[string]any{"from_node_id": "A", "to_node_id": "C", "repo_id": "r1", "branch": "main", "edge_kinds": []string{"CALLS", "IMPLEMENTS"}}
	resp, rpcErr := dispatchTracePath(t, r, withImpl)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Paths) != 1 || len(resp.Paths[0].Nodes) != 3 {
		t.Fatalf("CALLS+IMPLEMENTS should find A>I>C, got %+v", resp.Paths)
	}
}
