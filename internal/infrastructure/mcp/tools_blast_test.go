package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// blastFakeEdges/blastFakeNodes are local stubs — kept disjoint from the
// search-tool stubs so each test file is independently readable.

type blastFakeEdges struct {
	inbound  map[string][]string
	outbound map[string][]string
}

func (f *blastFakeEdges) InboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.inbound[id]...)
	}
	return out, nil
}

func (f *blastFakeEdges) OutboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.outbound[id]...)
	}
	return out, nil
}

type blastFakeNodes struct {
	metas  map[string]ports.NodeMeta
	byFile map[string][]string
}

func (f *blastFakeNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	var out []ports.NodeMeta
	for _, id := range ids {
		if m, ok := f.metas[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *blastFakeNodes) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	return f.byFile[filePath], nil
}

func dispatchBlast(t *testing.T, r *Registry, method string, params any) (BlastResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: method, Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return BlastResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp BlastResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

func TestBlastRadius_DefaultsToCallers(t *testing.T) {
	edges := &blastFakeEdges{inbound: map[string][]string{
		"seed": {"caller"},
	}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"seed":   {NodeID: "seed", SymbolPath: "S"},
		"caller": {NodeID: "caller", SymbolPath: "C"},
	}}
	svc := blastradius.NewService(edges, nodes, nil)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	resp, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"node_id":   "seed",
		"repo_id":   "r1",
		"branch":    "main",
		"max_depth": 1,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("expected seed + 1 caller, got %d (%+v)", len(resp.Entries), resp.Entries)
	}
	if resp.Entries[0].NodeID != "seed" || resp.Entries[1].NodeID != "caller" {
		t.Errorf("unexpected order: %+v", resp.Entries)
	}
}

func TestBlastRadius_HonoursCalleesDirection(t *testing.T) {
	edges := &blastFakeEdges{outbound: map[string][]string{
		"seed": {"callee"},
	}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"seed":   {NodeID: "seed"},
		"callee": {NodeID: "callee"},
	}}
	svc := blastradius.NewService(edges, nodes, nil)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	resp, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"node_id":   "seed",
		"repo_id":   "r1",
		"branch":    "main",
		"direction": "callees",
		"max_depth": 1,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Entries) != 2 || resp.Entries[1].NodeID != "callee" {
		t.Errorf("expected callee neighbour, got %+v", resp.Entries)
	}
}

func TestBlastRadius_BadDirectionRejected(t *testing.T) {
	svc := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"node_id":   "seed",
		"repo_id":   "r",
		"branch":    "main",
		"direction": "sideways",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestBlastRadius_RequiresParams(t *testing.T) {
	svc := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"repo_id": "r",
		"branch":  "main",
		// node_id missing
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestDirtyBlastRadius_FlagsIncludedStaging(t *testing.T) {
	staging := application.NewStagingArea()
	n, _ := domain.NewNode("s1", "foo.go", "Foo", domain.KindFunction)
	staging.StageFile("r1", "main", "foo.go", []*domain.Node{n}, nil)

	edges := &blastFakeEdges{inbound: map[string][]string{"s1": {"x"}}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"s1": {NodeID: "s1"}, "x": {NodeID: "x"},
	}}
	svc := blastradius.NewService(edges, nodes, staging)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	resp, rpcErr := dispatchBlast(t, r, "eng_get_dirty_blast_radius", map[string]any{
		"repo_id":   "r1",
		"branch":    "main",
		"max_depth": 1,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if !resp.IncludedStaging {
		t.Error("expected IncludedStaging=true")
	}
	if len(resp.Entries) != 2 {
		t.Errorf("expected staged seed + caller, got %+v", resp.Entries)
	}
}

func TestBlastTools_RegistersThreeTools(t *testing.T) {
	svc := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	got := r.Names()
	want := []string{"eng_get_blast_radius", "eng_get_diff_blast_radius", "eng_get_dirty_blast_radius"}
	if len(got) != len(want) {
		t.Fatalf("got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("at %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestDiffBlastRadius_NotWiredReturnsInternalError(t *testing.T) {
	svc := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "r", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}

func TestDiffBlastRadius_HappyPath(t *testing.T) {
	// We need blastFakeNodes to honour byFile too.
	edges := &blastFakeEdges{inbound: map[string][]string{"a": {"caller"}}}
	nodes := &blastFakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "caller": {NodeID: "caller"},
		},
		byFile: map[string][]string{"foo.go": {"a"}},
	}
	svc := blastradius.NewService(edges, nodes, nil)

	repoRoot := func(_ context.Context, _ string) (string, error) {
		return "/tmp/r", nil
	}
	changed := func(_ context.Context, _ string) ([]string, error) {
		return []string{"foo.go"}, nil
	}

	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil)
	resp, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id":   "r1",
		"branch":    "main",
		"max_depth": 1,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("expected seed + 1 caller, got %+v", resp.Entries)
	}
}

func TestDiffBlastRadius_UnknownRepo(t *testing.T) {
	svc := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	repoRoot := func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("no such repo")
	}
	changed := func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "ghost", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %+v", rpcErr)
	}
}
