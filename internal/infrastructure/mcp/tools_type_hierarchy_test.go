// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// dispatchTypeHierarchy dispatches a type-hierarchy tool and decodes the envelope.
func dispatchTypeHierarchy(t *testing.T, r *Registry, method string, params any) (typeHierarchyResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, &Request{Method: method, Params: json.RawMessage(raw)})
	if rpcErr != nil {
		return typeHierarchyResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var resp typeHierarchyResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal typeHierarchyResponse: %v", err)
	}
	return resp, nil
}

// typeHierarchyStore wires a small graph: interface Reader, concrete *File that
// IMPLEMENTS it, and a Server struct that EMBEDS File.
func typeHierarchyStore(t *testing.T) *stubGraphStorage {
	t.Helper()
	s := newStubGraphStorage()
	s.addNode(mustNode(t, "iface", "io/io.go", "Reader", domain.KindInterface))
	s.addNode(mustNode(t, "file", "os/file.go", "File", domain.KindStruct))
	s.addNode(mustNode(t, "server", "app/server.go", "Server", domain.KindStruct))
	s.addEdge(mustEdge(t, "file", "iface", domain.EdgeImplements))
	s.addEdge(mustEdge(t, "server", "file", domain.EdgeEmbeds))
	return s
}

// On an interface seed, eng_find_implementations returns the implementing types
// (incoming IMPLEMENTS).
func TestFindImplementations_InterfaceSeedReturnsImplementers(t *testing.T) {
	r := NewRegistry()
	RegisterGraphTools(r, typeHierarchyStore(t), staging.NewArea())

	resp, rpcErr := dispatchTypeHierarchy(t, r, "eng_find_implementations", map[string]string{
		"node_id": "iface", "repo_id": "r1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Name != "File" {
		t.Fatalf("interface seed should return implementer File, got %+v", resp.Nodes)
	}
	if len(resp.Edges) != 1 || resp.Edges[0].Kind != string(domain.EdgeImplements) {
		t.Fatalf("expected one IMPLEMENTS edge, got %+v", resp.Edges)
	}
}

// On a concrete-type seed, eng_find_implementations returns the interfaces it
// satisfies (outgoing IMPLEMENTS).
func TestFindImplementations_TypeSeedReturnsInterfaces(t *testing.T) {
	r := NewRegistry()
	RegisterGraphTools(r, typeHierarchyStore(t), staging.NewArea())

	resp, rpcErr := dispatchTypeHierarchy(t, r, "eng_find_implementations", map[string]string{
		"node_id": "file", "repo_id": "r1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Name != "Reader" {
		t.Fatalf("type seed should return satisfied interface Reader, got %+v", resp.Nodes)
	}
}

// eng_get_type_hierarchy surfaces both EMBEDS and IMPLEMENTS neighbors around a
// seed in one query.
func TestGetTypeHierarchy_ReturnsEmbedAndImplementNeighbors(t *testing.T) {
	r := NewRegistry()
	RegisterGraphTools(r, typeHierarchyStore(t), staging.NewArea())

	resp, rpcErr := dispatchTypeHierarchy(t, r, "eng_get_type_hierarchy", map[string]string{
		"node_id": "file", "repo_id": "r1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	// From File: outgoing IMPLEMENTS->Reader, incoming EMBEDS<-Server.
	kinds := map[string]bool{}
	for _, e := range resp.Edges {
		kinds[e.Kind] = true
	}
	if !kinds[string(domain.EdgeImplements)] || !kinds[string(domain.EdgeEmbeds)] {
		t.Fatalf("expected both IMPLEMENTS and EMBEDS edges, got %+v", resp.Edges)
	}
	names := map[string]bool{}
	for _, n := range resp.Nodes {
		names[n.Name] = true
	}
	if !names["Reader"] || !names["Server"] {
		t.Fatalf("expected Reader and Server in neighborhood, got %+v", resp.Nodes)
	}
}
