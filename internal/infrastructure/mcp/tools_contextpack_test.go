package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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
	blast := blastradius.NewService(edges, nodes, nil)

	findNodes := func(_ context.Context, _, _, sym string) ([]*domain.Node, error) {
		if sym == "Target" {
			n, _ := domain.NewNode("seed", "a.go", "Target", domain.KindFunction)
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
	a, err := contextpack.NewAssembler(findNodes, blast, fileHistory, openFindings, changedFiles, nodes.NodesInFile, activeTask, opts...)
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
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""))
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

func TestContextPack_TaskMode(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""))
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
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""))
	for _, params := range []map[string]any{
		{"repo_id": "r", "branch": "main"},
		{"repo_id": "r", "branch": "main", "symbol": "Target", "task_id": "t1"},
	} {
		_, rpcErr := dispatchContextPack(t, r, params)
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Fatalf("want CodeInvalidParams for %v, got %+v", params, rpcErr)
		}
	}
}

func TestContextPack_Truncation(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t, contextpack.WithTokenBudget(1)), stubRepoRoot(""))
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
	RegisterContextPackTool(r, nil, nil)
	_, rpcErr := dispatchContextPack(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "symbol": "Target",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("want CodeInternalError when unwired, got %+v", rpcErr)
	}
}

func TestContextPack_P95Latency(t *testing.T) {
	r := NewRegistry()
	RegisterContextPackTool(r, contextPackFixture(t), stubRepoRoot(""))
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
