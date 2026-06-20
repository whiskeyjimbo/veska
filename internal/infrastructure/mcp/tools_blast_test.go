// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// These fake edge and node stubs are kept disjoint from other test files to maintain isolated readability.

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
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
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

func TestBlastRadius_HonorsCalleesDirection(t *testing.T) {
	edges := &blastFakeEdges{outbound: map[string][]string{
		"seed": {"callee"},
	}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"seed":   {NodeID: "seed"},
		"callee": {NodeID: "callee"},
	}}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
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
		t.Errorf("expected callee neighbor, got %+v", resp.Entries)
	}
}

// When only the inbound resolver is wired, the blast radius response must include cross-repository edges from callers in other repositories targeting the node.
func TestBlastRadius_InboundResolverSurfacesCrossRepoCallers(t *testing.T) {
	edges := &blastFakeEdges{inbound: map[string][]string{}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"lib-seed": {NodeID: "lib-seed", SymbolPath: "lib.Hello"},
	}}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	inbound := func(_ context.Context, dst, _ string) ([]ports.ResolvedEdge, error) {
		if dst != "lib-seed" {
			return nil, nil
		}
		return []ports.ResolvedEdge{{
			SrcNodeID: "app-caller", DstNodeID: "lib-seed",
			DstRepoID: "lib-repo", DstBranch: "main", Kind: "CALLS",
		}}, nil
	}
	RegisterBlastTools(r, svc, nil, nil, nil, nil,
		WithBlastInboundResolveFunc(inbound))
	resp, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"node_id":   "lib-seed",
		"repo_id":   "lib-repo",
		"branch":    "main",
		"direction": "callers",
		"max_depth": 1,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.CrossRepoEdges) != 1 {
		t.Fatalf("want 1 cross_repo_edge, got %d: %+v", len(resp.CrossRepoEdges), resp.CrossRepoEdges)
	}
	e := resp.CrossRepoEdges[0]
	if e.SrcNodeID != "app-caller" || e.DstNodeID != "lib-seed" || !e.CrossRepo {
		t.Errorf("edge mismatch: %+v", e)
	}
}

// The cross-repository inbound resolver must not be invoked when querying the callees direction, as inbound callers are not part of the transitive callee set.
func TestBlastRadius_InboundResolverSkippedForCalleesDirection(t *testing.T) {
	edges := &blastFakeEdges{outbound: map[string][]string{}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"seed": {NodeID: "seed"},
	}}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	calls := 0
	inbound := func(_ context.Context, _ string, _ string) ([]ports.ResolvedEdge, error) {
		calls++
		return nil, nil
	}
	RegisterBlastTools(r, svc, nil, nil, nil, nil, WithBlastInboundResolveFunc(inbound))
	_, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"node_id":   "seed",
		"repo_id":   "r1",
		"branch":    "main",
		"direction": "callees",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if calls != 0 {
		t.Errorf("inbound resolver must not be called for direction=callees; got %d calls", calls)
	}
}

func TestBlastRadius_BadDirectionRejected(t *testing.T) {
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
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
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"repo_id": "r",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestDirtyBlastRadius_FlagsIncludedStaging(t *testing.T) {
	area := staging.NewArea()
	n, _ := domain.NewNode(domain.NodeSpec{ID: "s1", Path: "foo.go", Name: "Foo", Kind: domain.KindFunction})
	area.Stage("r1", "main", "foo.go", staging.File{Nodes: []*domain.Node{n}, Edges: nil})

	edges := &blastFakeEdges{inbound: map[string][]string{"s1": {"x"}}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"s1": {NodeID: "s1"}, "x": {NodeID: "x"},
	}}
	svc, err := blastradius.NewService(edges, nodes, area)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
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
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
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
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "r", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}

func TestDiffBlastRadius_HappyPath(t *testing.T) {
	edges := &blastFakeEdges{inbound: map[string][]string{"a": {"caller"}}}
	nodes := &blastFakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "caller": {NodeID: "caller"},
		},
		byFile: map[string][]string{"foo.go": {"a"}},
	}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	repoRoot := func(_ context.Context, _ string) (string, error) {
		return "/tmp/r", nil
	}
	changed := func(_ context.Context, _ string) ([]string, error) {
		return []string{"foo.go"}, nil
	}

	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil, nil)
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
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	repoRoot := func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("no such repo")
	}
	changed := func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil, nil)
	_, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "ghost", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %+v", rpcErr)
	}
}

// When both ref_a and ref_b are provided, the handler retrieves changes between those specific revisions rather than using the active working-tree files list.
func TestDiffBlastRadius_RangedRefs(t *testing.T) {
	edges := &blastFakeEdges{inbound: map[string][]string{"a": {"caller"}}}
	nodes := &blastFakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "caller": {NodeID: "caller"},
		},
		byFile: map[string][]string{"foo.go": {"a"}},
	}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	repoRoot := func(_ context.Context, _ string) (string, error) { return "/tmp/r", nil }
	workingTreeCalled := false
	changed := func(_ context.Context, _ string) ([]string, error) {
		workingTreeCalled = true
		return nil, nil
	}
	var gotA, gotB string
	between := func(_ context.Context, _, refA, refB string) ([]string, error) {
		gotA, gotB = refA, refB
		return []string{"foo.go"}, nil
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil, nil, WithBlastChangedFilesBetween(between))
	resp, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "r1", "branch": "main", "max_depth": 1,
		"ref_a": "v1.0.0", "ref_b": "HEAD",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if gotA != "v1.0.0" || gotB != "HEAD" {
		t.Fatalf("between got refs (%q,%q), want (v1.0.0,HEAD)", gotA, gotB)
	}
	if workingTreeCalled {
		t.Fatal("working-tree changedFiles must not be called when refs are supplied")
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("expected seed + 1 caller, got %+v", resp.Entries)
	}
}

// The ref_a and ref_b parameters are validated as all-or-nothing; specifying one without the other is rejected.
func TestDiffBlastRadius_LoneRef(t *testing.T) {
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	repoRoot := func(_ context.Context, _ string) (string, error) { return "/tmp/r", nil }
	changed := func(_ context.Context, _ string) ([]string, error) { return nil, nil }
	between := func(_ context.Context, _, _, _ string) ([]string, error) { return nil, nil }
	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil, nil, WithBlastChangedFilesBetween(between))
	_, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "r1", "branch": "main", "ref_a": "v1.0.0",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for lone ref_a, got %+v", rpcErr)
	}
}

// We map git unknown-revision errors to CodeInvalidParams to indicate that the user provided a malformed or non-existent git reference.
func TestDiffBlastRadius_UnknownRevision(t *testing.T) {
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	repoRoot := func(_ context.Context, _ string) (string, error) { return "/tmp/r", nil }
	changed := func(_ context.Context, _ string) ([]string, error) { return nil, nil }
	between := func(_ context.Context, _, _, _ string) ([]string, error) {
		return nil, fmt.Errorf("%w: refs=bogus..HEAD", gitinfra.ErrUnknownRevision)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, repoRoot, changed, nil, nil, WithBlastChangedFilesBetween(between))
	_, rpcErr := dispatchBlast(t, r, "eng_get_diff_blast_radius", map[string]any{
		"repo_id": "r1", "branch": "main", "ref_a": "bogus", "ref_b": "HEAD",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown revision, got %+v", rpcErr)
	}
}

// When only a symbol name is supplied, the blast radius query resolves the symbol name to its node ID using graph storage before calculating the radius.
func TestBlastRadius_AcceptsSymbol(t *testing.T) {
	edges := &blastFakeEdges{inbound: map[string][]string{
		"n1": {"caller"},
	}}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{
		"n1":     {NodeID: "n1", SymbolPath: "Foo"},
		"caller": {NodeID: "caller", SymbolPath: "C"},
	}}
	svc, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	graph := newStubGraphStorage()
	graph.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, graph)

	resp, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"symbol":  "Foo",
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected err: %+v", rpcErr)
	}
	if len(resp.Entries) != 2 || resp.Entries[0].NodeID != "n1" {
		t.Errorf("expected seed=n1 + caller, got %+v", resp.Entries)
	}
}

// An ambiguous symbol name that resolves to multiple nodes in the graph storage is rejected with a CodeInvalidParams error.
func TestBlastRadius_AmbiguousSymbolRejected(t *testing.T) {
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	graph := newStubGraphStorage()
	graph.addNode(mustNode(t, "a", "a.go", "Foo", domain.KindFunction))
	graph.addNode(mustNode(t, "b", "b.go", "Foo", domain.KindFunction))
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, graph)

	_, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"symbol":  "Foo",
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for ambiguous symbol, got %+v", rpcErr)
	}
}

// A blast radius request is rejected if both the node ID and symbol parameters are omitted.
func TestBlastRadius_MissingNodeAndSymbol(t *testing.T) {
	svc, err := blastradius.NewService(&blastFakeEdges{}, &blastFakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterBlastTools(r, svc, nil, nil, nil, newStubGraphStorage())
	_, rpcErr := dispatchBlast(t, r, "eng_get_blast_radius", map[string]any{
		"repo_id": "r1", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %+v", rpcErr)
	}
}
