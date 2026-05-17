package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// entryPointsFixtureService builds an EntryPointsService over a fixed
// promoted state so the MCP handler test asserts the tool exposes the
// same data the page is built from (AC3).
func entryPointsFixtureService(t *testing.T) *wiki.EntryPointsService {
	t.Helper()
	g, err := domain.NewGraph("r1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	mk := func(id, path string, kind domain.NodeKind) {
		n, err := domain.NewNode(id, path, id, kind)
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
	blast := blastradius.NewService(
		&blastFakeEdges{inbound: inbound},
		&blastFakeNodes{metas: map[string]ports.NodeMeta{
			"low": {NodeID: "low"}, "low_test": {NodeID: "low_test"},
		}},
		nil,
	)
	svc, err := wiki.NewEntryPointsService(loadGraph, inboundEdges, openFindings, blast)
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

// AC3: the tool returns the same data the page exposes.
func TestEntryPoints_ReturnsSelectedData(t *testing.T) {
	svc := entryPointsFixtureService(t)
	r := NewRegistry()
	RegisterEntryPointsTool(r, svc)

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

	// The tool data must match what the page is rendered from.
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

func TestEntryPoints_RequiresParams(t *testing.T) {
	svc := entryPointsFixtureService(t)
	r := NewRegistry()
	RegisterEntryPointsTool(r, svc)

	_, rpcErr := dispatchEntryPoints(t, r, map[string]any{"repo_id": "r1"})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestEntryPoints_NotWiredReturnsInternalError(t *testing.T) {
	r := NewRegistry()
	RegisterEntryPointsTool(r, nil)

	_, rpcErr := dispatchEntryPoints(t, r, map[string]any{"repo_id": "r1", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}
