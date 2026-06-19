// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// We build the EntryPointsService over a fixed promoted state to verify that the tool exposes the same data used to construct the wiki page.
func entryPointsFixtureService(t *testing.T) *wiki.EntryPointsService {
	t.Helper()
	g, err := domain.NewGraph("r1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	mk := func(id, path string, kind domain.NodeKind) {
		n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: id, Kind: kind})
		if err != nil {
			t.Fatalf("NewNode: %v", err)
		}
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	mk("low", "app/low.go", domain.KindFunction)
	mk("low_test", "app/low_test.go", domain.KindTest)

	loadGraph := func(context.Context, string, string) (*domain.Graph, error) { return g, nil }
	inbound := map[string][]string{"low": {"low_test"}}
	inboundEdges := func(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
		out := make(map[string][]string, len(ids))
		for _, id := range ids {
			out[id] = append([]string(nil), inbound[id]...)
		}
		return out, nil
	}
	openFindings := func(context.Context, string, string) (map[string]bool, error) {
		return map[string]bool{}, nil
	}
	svc, err := wiki.NewEntryPointsService(loadGraph, inboundEdges, openFindings)
	if err != nil {
		t.Fatalf("NewEntryPointsService: %v", err)
	}
	return svc
}

func dispatchEntryPoints(t *testing.T, r *Registry, params any) (EntryPointsResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: "eng_get_entry_points", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return EntryPointsResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp EntryPointsResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

func TestEntryPoints_ReturnsSelectedData(t *testing.T) {
	svc := entryPointsFixtureService(t)
	r := NewRegistry()
	RegisterEntryPointsTool(r, svc, nil)

	resp, rpcErr := dispatchEntryPoints(t, r, map[string]any{
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.EntryPoints) != 1 {
		t.Fatalf("expected 1 entry point, got %d (%+v)", len(resp.EntryPoints), resp.EntryPoints)
	}
	if resp.EntryPoints[0].SymbolName != "low" {
		t.Errorf("entry[0]: got %+v, want symbol low", resp.EntryPoints[0])
	}

	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for i := range rep.EntryPoints {
		if resp.EntryPoints[i] != rep.EntryPoints[i] {
			t.Errorf("entry %d diverges: tool=%+v page=%+v", i, resp.EntryPoints[i], rep.EntryPoints[i])
		}
	}
}

// The default filter excludes test files and test-prefixed symbol names unless include_tests is explicitly enabled.
func TestFilterTestEntries_DefaultDropsTestSymbols(t *testing.T) {
	in := []wiki.EntryPoint{
		{SymbolName: "Execute", FilePath: "cobra/command.go"},
		{SymbolName: "TestExecute", FilePath: "cobra/command_test.go"},
		{SymbolName: "BenchmarkAddCommand", FilePath: "cobra/command_test.go"},
		{SymbolName: "ExampleCommand", FilePath: "cobra/example_test.go"},
		{SymbolName: "FuzzParseFlag", FilePath: "cobra/fuzz_test.go"},
		{SymbolName: "AddCommand", FilePath: "cobra/command.go"},
		{SymbolName: "helper", FilePath: "cobra/command_test.go"},
	}
	out := filterTestEntries(in)
	if len(out) != 2 || out[0].SymbolName != "Execute" || out[1].SymbolName != "AddCommand" {
		t.Errorf("filter dropped wrong entries; kept: %+v", out)
	}
}

func TestEntryPoints_IncludeTestsFlagRoundtrips(t *testing.T) {
	svc := entryPointsFixtureService(t)
	r := NewRegistry()
	RegisterEntryPointsTool(r, svc, nil)

	resp, rpcErr := dispatchEntryPoints(t, r, map[string]any{
		"repo_id": "r1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.EntryPoints) != 1 {
		t.Fatalf("default: expected 1 entry, got %d", len(resp.EntryPoints))
	}

	resp2, rpcErr := dispatchEntryPoints(t, r, map[string]any{
		"repo_id": "r1", "branch": "main", "include_tests": true,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp2.EntryPoints) != 1 {
		t.Fatalf("include_tests=true: expected 1 entry, got %d", len(resp2.EntryPoints))
	}
}

func TestEntryPoints_RequiresParams(t *testing.T) {
	svc := entryPointsFixtureService(t)
	r := NewRegistry()
	RegisterEntryPointsTool(r, svc, nil)

	_, rpcErr := dispatchEntryPoints(t, r, map[string]any{"repo_id": "r1"})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestEntryPoints_NotWiredReturnsInternalError(t *testing.T) {
	r := NewRegistry()
	RegisterEntryPointsTool(r, nil, nil)

	_, rpcErr := dispatchEntryPoints(t, r, map[string]any{"repo_id": "r1", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}
