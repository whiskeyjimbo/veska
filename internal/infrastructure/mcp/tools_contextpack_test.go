package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

func contextPackFixture(t *testing.T, opts ...contextpack.Option) *contextpack.Assembler {
	t.Helper()
	edges := &blastFakeEdges{inbound: map[string][]string{
		"seed": {"caller1"},
	}}
	nodes := &blastFakeNodes{
		metas: map[string]ports.NodeMeta{
			"seed":    {NodeID: "seed", SymbolPath: "pkg.Target", FilePath: "a.go", Kind: "function"},
			"caller1": {NodeID: "caller1", SymbolPath: "pkg.Caller", FilePath: "b.go", Kind: "function"},
		},
		byFile: map[string][]string{"a.go": {"seed"}, "b.go": {"caller1"}},
	}
	blast, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	findNodes := func(_ context.Context, _, _, sym string) ([]*domain.Node, error) {
		if sym == "Target" {
			n, _ := domain.NewNode(domain.NodeSpec{ID: "seed", Path: "a.go", Name: "Target", Kind: domain.KindFunction})
			return []*domain.Node{n}, nil
		}
		return nil, nil
	}
	fileHistory := func(_ context.Context, _, path string, _ time.Duration) ([]contextpack.CommitInfo, error) {
		return []contextpack.CommitInfo{{Hash: "h-" + path, Author: "dev", When: time.Unix(1000, 0), Subject: "s"}}, nil
	}
	openFindings := func(_ context.Context, _, _ string) (map[string]bool, error) {
		return map[string]bool{"caller1": true}, nil
	}
	changedFiles := func(_ context.Context, _ string) ([]string, error) {
		return []string{"a.go"}, nil
	}
	activeTask := func(_ context.Context, repoID string) (*contextpack.TaskInfo, error) {
		return &contextpack.TaskInfo{TaskID: "t1", RepoID: repoID, Title: "work", Active: true}, nil
	}
	a, err := contextpack.NewAssembler(contextpack.AssemblerDeps{
		FindNodes:    findNodes,
		Blast:        blast,
		FileHistory:  fileHistory,
		OpenFindings: openFindings,
		ChangedFiles: changedFiles,
		NodesInFile:  nodes.NodesInFile,
		ActiveTask:   activeTask,
	}, opts...)
	if err != nil {
		t.Fatalf("NewAssembler: %v", err)
	}
	return a
}

func dispatchContextPack(t *testing.T, r *Registry, params any) (contextpack.Pack, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: "eng_get_context_pack", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return contextpack.Pack{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var pack contextpack.Pack
	if err := json.Unmarshal(b, &pack); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return pack, nil
}

func stubRepoRoot(string) RepoRootFunc {
	return func(context.Context, string) (string, error) { return "/repo", nil }
}

func TestContextPack_SymbolMode(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil)
	pack, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "symbol": "Target",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	if pack.Mode != "symbol" || len(pack.Nodes) != 2 {
		t.Fatalf("symbol mode pack wrong: %+v", pack)
	}
	if len(pack.RecentCommits) == 0 || len(pack.OpenFindings) != 1 || len(pack.Tasks) != 1 {
		t.Fatalf("missing sections: %+v", pack)
	}
}

// TestContextPack_AcceptsShortID guards the README contract that a short_id
// prefix resolves anywhere a repo_id is required . Before the fix
// context_pack rejected the prefix with "repo not found".
func TestContextPack_AcceptsShortID(t *testing.T) {
	const fullID = "62d72fa222a0193f8fa927f95dd6a3575c7566964c8b8f6ba14aafc5a1ea871f"
	r := NewRegistry()
	repos := &fakeRepoLister{recs: []application.RepoRecord{{RepoID: fullID, RootPath: "/repo", ActiveBranch: "main"}}}
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), repos)
	pack, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": ShortRepoID(fullID), "branch": "main", "symbol": "Target",
	})
	if rpcErr != nil {
		t.Fatalf("short_id rejected: %+v", rpcErr)
	}
	if pack.Mode != "symbol" {
		t.Fatalf("want symbol mode via short_id, got %q", pack.Mode)
	}
}

// TestContextPack_CrossRepoEdges guards solov2-7xrw: when a resolver is
// wired and any node in the pack has cross_repo_edge_stubs, the response
// must surface them in a top-level cross_repo_edges array — parity with
// eng_get_call_chain and eng_get_blast_radius. Without it the CLI 'context'
// wrapper has no signal that consumers exist in other repos.
func TestContextPack_CrossRepoEdges(t *testing.T) {
	r := NewRegistry()
	resolve := func(_ context.Context, nodeID, _ string, _ bool) ([]ports.ResolvedEdge, error) {
		if nodeID != "seed" {
			return nil, nil
		}
		return []ports.ResolvedEdge{{
			SrcNodeID: "seed",
			DstNodeID: "remote-node",
			DstRepoID: "other-repo",
			DstBranch: "main",
			Kind:      "CALLS",
		}}, nil
	}
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil,
		WithContextPackResolveFunc(resolve))
	raw, _ := json.Marshal(map[string]any{"repo_id": "r", "branch": "main", "symbol": "Target"})
	req := &Request{Method: "eng_get_context_pack", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	var resp struct {
		Nodes          []map[string]any  `json:"nodes"`
		CrossRepoEdges []json.RawMessage `json:"cross_repo_edges"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.CrossRepoEdges) != 1 {
		t.Fatalf("want 1 cross_repo_edge, got %d: %s", len(resp.CrossRepoEdges), string(b))
	}
	var edge struct {
		SrcNodeID string `json:"src_node_id"`
		DstNodeID string `json:"dst_node_id"`
		DstRepoID string `json:"dst_repo_id"`
		Kind      string `json:"kind"`
		CrossRepo bool   `json:"cross_repo"`
	}
	if err := json.Unmarshal(resp.CrossRepoEdges[0], &edge); err != nil {
		t.Fatalf("edge unmarshal: %v", err)
	}
	if edge.SrcNodeID != "seed" || edge.DstNodeID != "remote-node" || edge.DstRepoID != "other-repo" || edge.Kind != "CALLS" || !edge.CrossRepo {
		t.Fatalf("edge mismatch: %+v", edge)
	}
}

// TestContextPack_NoResolverOmitsCrossRepo: without WithContextPackResolveFunc
// the response must NOT include a cross_repo_edges field, so older clients
// see exactly the same JSON shape they did before solov2-7xrw.
func TestContextPack_NoResolverOmitsCrossRepo(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil)
	raw, _ := json.Marshal(map[string]any{"repo_id": "r", "branch": "main", "symbol": "Target"})
	req := &Request{Method: "eng_get_context_pack", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	if contains(string(b), "cross_repo_edges") {
		t.Fatalf("want no cross_repo_edges field when resolver unwired, got %s", string(b))
	}
}

func TestContextPack_TaskMode(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil)
	pack, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "task_id": "t1",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	if pack.Mode != "task" || len(pack.Nodes) != 2 {
		t.Fatalf("task mode pack wrong: %+v", pack)
	}
}

func TestContextPack_RejectsBothOrNeither(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil)
	for _, params := range []map[string]any{
		{"repo_id": "r", "branch": "main"},
		{"repo_id": "r", "branch": "main", "symbol": "Target", "task_id": "t1"},
		{"repo_id": "r", "branch": "main", "symbol": "Target", "node_id": "seed"},
		{"repo_id": "r", "branch": "main", "task_id": "t1", "node_id": "seed"},
		{"repo_id": "r", "branch": "main", "symbol": "Target", "task_id": "t1", "node_id": "seed"},
	} {
		_, rpcErr := dispatchContextPack(t, r, params)
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Fatalf("want CodeInvalidParams for %v, got %+v", params, rpcErr)
		}
	}
}

// TestContextPack_NodeMode covers solov2-z81b: agents that already hold a
// node_id (from eng_find_symbol / eng_search_semantic) can anchor directly
// without a symbol round-trip.
func TestContextPack_NodeMode(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil)
	pack, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "node_id": "seed",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	if pack.Mode != "node" {
		t.Fatalf("want node mode, got %q", pack.Mode)
	}
	if len(pack.Nodes) != 2 {
		t.Fatalf("want 2 nodes (seed + caller via blast), got %d: %+v", len(pack.Nodes), pack.Nodes)
	}
}

// TestContextPack_SchemaDeclaresNodeID guards the schema-vs-behaviour parity
// invariant: the inputSchema must declare every accepted param, and the
// description must name all three anchors so agents discover the option
// without reading source. additionalProperties:false must remain.
func TestContextPack_SchemaDeclaresNodeID(t *testing.T) {
	var schema struct {
		AdditionalProperties any    `json:"additionalProperties"`
		Description          string `json:"description"`
		Properties           map[string]any
	}
	if err := json.Unmarshal(contextPackInputSchema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if _, ok := schema.Properties["node_id"]; !ok {
		t.Fatal("schema is missing node_id property")
	}
	if _, ok := schema.Properties["symbol"]; !ok {
		t.Fatal("schema is missing symbol property")
	}
	if _, ok := schema.Properties["task_id"]; !ok {
		t.Fatal("schema is missing task_id property")
	}
	if b, ok := schema.AdditionalProperties.(bool); !ok || b {
		t.Fatalf("want additionalProperties:false, got %v", schema.AdditionalProperties)
	}
	for _, anchor := range []string{"node_id", "symbol", "task_id"} {
		if !contains(schema.Description, anchor) {
			t.Fatalf("schema description must name %q; got %q", anchor, schema.Description)
		}
	}
}

func TestContextPack_Truncation(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t, contextpack.WithTokenBudget(1)), stubRepoRoot(""), nil)
	pack, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "symbol": "Target",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	if !pack.Truncated {
		t.Fatal("want truncated bundle, not rejection")
	}
}

func TestContextPack_NotWired(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, nil, nil, nil)
	_, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "symbol": "Target",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("want CodeInternalError when unwired, got %+v", rpcErr)
	}
}

func TestContextPack_P95Latency(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""), nil)
	const iter = 200
	durs := make([]time.Duration, iter)
	for i := range durs {
		start := time.Now()
		if _, rpcErr := dispatchContextPack(t, r, map[string]any{
			"repo_id": "r", "branch": "main", "symbol": "Target",
		}); rpcErr != nil {
			t.Fatalf("dispatch: %+v", rpcErr)
		}
		durs[i] = time.Since(start)
	}
	for i := 1; i < len(durs); i++ {
		for j := i; j > 0 && durs[j-1] > durs[j]; j-- {
			durs[j-1], durs[j] = durs[j], durs[j-1]
		}
	}
	p95 := durs[(95*len(durs))/100]
	if p95 > 50*time.Millisecond {
		t.Fatalf("p95 latency %v exceeds 50ms", p95)
	}
}
