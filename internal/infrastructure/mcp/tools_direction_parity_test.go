// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"reflect"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// These tests pin the direction-enum parity contract (solov2-py5m): the three
// graph-traversal tools must accept BOTH vocabularies as aliases -
// in==callers (inbound), out==callees (outbound), both==both - and produce
// identical results for the equivalent pair, while still rejecting unknown
// values with CodeInvalidParams.

func newBlastParitySvc(t *testing.T) *blastradius.Service {
	t.Helper()
	edges := &blastFakeEdges{
		inbound:  map[string][]string{"seed": {"caller"}},
		outbound: map[string][]string{"seed": {"callee"}},
	}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"seed":   {NodeID: "seed", SymbolPath: "S"},
		"caller": {NodeID: "caller", SymbolPath: "C"},
		"callee": {NodeID: "callee", SymbolPath: "E"},
	}}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return svc
}

// callChainResult dispatches eng_get_call_chain with the given direction.
func callChainResult(t *testing.T, dir string) (callChainResponse, *RPCError) {
	t.Helper()
	store := newStubGraphStorage()
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
	params := map[string]any{"node_id": "b", "repo_id": "repo1", "branch": "main"}
	if dir != "" {
		params["direction"] = dir
	}
	return dispatchCallChain(t, r, "eng_get_call_chain", params)
}

func TestCallChain_DirectionAliasParity(t *testing.T) {
	pairs := []struct{ a, b string }{
		{"in", "callers"},
		{"out", "callees"},
		{"both", "both"},
	}
	for _, pr := range pairs {
		ra, errA := callChainResult(t, pr.a)
		if errA != nil {
			t.Fatalf("direction %q rejected: %+v", pr.a, errA)
		}
		rb, errB := callChainResult(t, pr.b)
		if errB != nil {
			t.Fatalf("direction %q rejected: %+v", pr.b, errB)
		}
		if !reflect.DeepEqual(ra, rb) {
			t.Errorf("call_chain %q vs %q diverge:\n%+v\n%+v", pr.a, pr.b, ra, rb)
		}
	}
}

// blastResult dispatches a blast-family tool with the given direction.
func blastResult(t *testing.T, method, dir string) (BlastResponse, *RPCError) {
	t.Helper()
	r := NewRegistry()
	switch method {
	case "eng_get_diff_blast_radius":
		edges := &blastFakeEdges{
			inbound:  map[string][]string{"seed": {"caller"}},
			outbound: map[string][]string{"seed": {"callee"}},
		}
		nodes := &blastFakeNodes{
			metas: map[string]ports.NodeMeta{
				"seed":   {NodeID: "seed", SymbolPath: "S"},
				"caller": {NodeID: "caller", SymbolPath: "C"},
				"callee": {NodeID: "callee", SymbolPath: "E"},
			},
			byFile: map[string][]string{"foo.go": {"seed"}},
		}
		svc, err := blastradius.NewService(edges, nodes, nil)
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		repoRoot := func(_ context.Context, _ string) (string, error) { return "/tmp/r", nil }
		changed := func(_ context.Context, _ string) ([]string, error) { return []string{"foo.go"}, nil }
		RegisterBlastTools(r, svc, repoRoot, changed, nil, nil)
	default:
		RegisterBlastTools(r, newBlastParitySvc(t), nil, nil, nil, nil)
	}
	params := map[string]any{"node_id": "seed", "repo_id": "r1", "branch": "main", "max_depth": 1}
	if method == "eng_get_diff_blast_radius" || method == "eng_get_dirty_blast_radius" {
		delete(params, "node_id")
	}
	if dir != "" {
		params["direction"] = dir
	}
	return dispatchBlast(t, r, method, params)
}

func TestBlastTools_DirectionAliasParity(t *testing.T) {
	// dirty blast needs a staged seed; exercise it separately below.
	methods := []string{"eng_get_blast_radius", "eng_get_diff_blast_radius"}
	pairs := []struct{ a, b string }{
		{"callers", "in"},
		{"callees", "out"},
		{"both", "both"},
	}
	for _, m := range methods {
		for _, pr := range pairs {
			ra, errA := blastResult(t, m, pr.a)
			if errA != nil {
				t.Fatalf("%s direction %q rejected: %+v", m, pr.a, errA)
			}
			rb, errB := blastResult(t, m, pr.b)
			if errB != nil {
				t.Fatalf("%s direction %q rejected: %+v", m, pr.b, errB)
			}
			if !reflect.DeepEqual(ra, rb) {
				t.Errorf("%s %q vs %q diverge:\n%+v\n%+v", m, pr.a, pr.b, ra, rb)
			}
		}
	}
}

func TestDirtyBlastRadius_DirectionAliasParity(t *testing.T) {
	build := func(dir string) (BlastResponse, *RPCError) {
		area := staging.NewArea()
		n, _ := domain.NewNode(domain.NodeSpec{ID: "seed", Path: "foo.go", Name: "Foo", Kind: domain.KindFunction})
		area.Stage("r1", "main", "foo.go", staging.File{Nodes: []*domain.Node{n}})
		edges := &blastFakeEdges{
			inbound:  map[string][]string{"seed": {"caller"}},
			outbound: map[string][]string{"seed": {"callee"}},
		}
		nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
			"seed":   {NodeID: "seed"},
			"caller": {NodeID: "caller"},
			"callee": {NodeID: "callee"},
		}}
		svc, err := blastradius.NewService(edges, nodes, area)
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		r := NewRegistry()
		RegisterBlastTools(r, svc, nil, nil, nil, nil)
		params := map[string]any{"repo_id": "r1", "branch": "main", "max_depth": 1}
		if dir != "" {
			params["direction"] = dir
		}
		return dispatchBlast(t, r, "eng_get_dirty_blast_radius", params)
	}
	pairs := []struct{ a, b string }{
		{"callers", "in"},
		{"callees", "out"},
		{"both", "both"},
	}
	for _, pr := range pairs {
		ra, errA := build(pr.a)
		if errA != nil {
			t.Fatalf("dirty direction %q rejected: %+v", pr.a, errA)
		}
		rb, errB := build(pr.b)
		if errB != nil {
			t.Fatalf("dirty direction %q rejected: %+v", pr.b, errB)
		}
		if !reflect.DeepEqual(ra, rb) {
			t.Errorf("dirty %q vs %q diverge:\n%+v\n%+v", pr.a, pr.b, ra, rb)
		}
	}
}

func TestDirectionTools_RejectUnknownAcrossAll(t *testing.T) {
	if _, rpcErr := callChainResult(t, "sideways"); rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Errorf("call_chain: want CodeInvalidParams, got %+v", rpcErr)
	}
	for _, m := range []string{"eng_get_blast_radius", "eng_get_diff_blast_radius", "eng_get_dirty_blast_radius"} {
		if _, rpcErr := blastResult(t, m, "sideways"); rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Errorf("%s: want CodeInvalidParams, got %+v", m, rpcErr)
		}
	}
}
