package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// wikiFixtureService mirrors the wiki package fixture so the MCP handler
// test asserts the tool exposes the same ranked data the page is built
// from (AC3).
func wikiFixtureService(t *testing.T) *wiki.HotZoneService {
	t.Helper()
	edges := &blastFakeEdges{inbound: map[string][]string{
		"a": {"x", "y"},
		"c": {"x", "y"},
	}}
	nodes := &blastFakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "b": {NodeID: "b"}, "c": {NodeID: "c"},
			"x": {NodeID: "x"}, "y": {NodeID: "y"},
		},
		byFile: map[string][]string{
			"/tmp/r/a.go": {"a"}, "/tmp/r/b.go": {"b"}, "/tmp/r/c.go": {"c"},
		},
	}
	blast := blastradius.NewService(edges, nodes, nil)
	counts := func(context.Context, string) (map[string]int, error) {
		return map[string]int{"a.go": 5, "b.go": 5, "c.go": 3}, nil
	}
	svc, err := wiki.NewHotZoneService(counts, nodes.NodesInFile, blast)
	if err != nil {
		t.Fatalf("NewHotZoneService: %v", err)
	}
	return svc
}

func dispatchHotZone(t *testing.T, r *Registry, params any) (HotZoneResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: "eng_get_hot_zone", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return HotZoneResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp HotZoneResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

// AC3: the tool returns the same ranked data the page exposes.
func TestHotZone_ReturnsRankedData(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }

	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot)

	resp, rpcErr := dispatchHotZone(t, r, map[string]any{
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Zones) != 3 {
		t.Fatalf("expected 3 zones, got %d (%+v)", len(resp.Zones), resp.Zones)
	}
	if resp.Zones[0].FilePath != "a.go" || resp.Zones[0].Score != 15 {
		t.Errorf("zone[0]: got %+v, want a.go score 15", resp.Zones[0])
	}
	if resp.Zones[2].FilePath != "b.go" {
		t.Errorf("zone[2]: got %+v, want b.go", resp.Zones[2])
	}

	// The tool data must match what the page renders.
	rep, err := svc.Rank(context.Background(), "r1", "main", "/tmp/r")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	for i := range rep.Zones {
		if resp.Zones[i] != rep.Zones[i] {
			t.Errorf("zone %d diverges: tool=%+v page=%+v", i, resp.Zones[i], rep.Zones[i])
		}
	}
}

func TestHotZone_RequiresParams(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }
	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot)

	_, rpcErr := dispatchHotZone(t, r, map[string]any{"repo_id": "r1"})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestHotZone_UnknownRepo(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) {
		return "", fmt.Errorf("no such repo")
	}
	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot)

	_, rpcErr := dispatchHotZone(t, r, map[string]any{"repo_id": "ghost", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %+v", rpcErr)
	}
}

func TestHotZone_NotWiredReturnsInternalError(t *testing.T) {
	r := NewRegistry()
	RegisterWikiTools(r, nil, nil)

	_, rpcErr := dispatchHotZone(t, r, map[string]any{"repo_id": "r1", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}
