package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type fakeStubAggregator struct {
	rows []dependencies.StubRow
}

func (f *fakeStubAggregator) AggregateStubs(_ context.Context, _, _ string) ([]dependencies.StubRow, error) {
	return f.rows, nil
}

func TestListDependencies_RanksByUsageCount(t *testing.T) {
	agg := &fakeStubAggregator{rows: []dependencies.StubRow{
		{ModulePath: "github.com/spf13/cobra", SymbolPath: "Command", SrcNodeID: "a", Language: "go"},
		{ModulePath: "github.com/spf13/cobra", SymbolPath: "AddCommand", SrcNodeID: "b", Language: "go"},
		{ModulePath: "golang.org/x/mod", SymbolPath: "Parse", SrcNodeID: "c", Language: "go"},
	}}
	svc, err := dependencies.NewService(agg, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r := NewRegistry()
	RegisterDependenciesTool(r, svc, nil)

	raw, _ := json.Marshal(map[string]any{"repo_id": "r1", "branch": "main"})
	req := &Request{Method: "eng_list_dependencies", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	var resp dependencies.Result
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Dependencies) != 2 {
		t.Fatalf("want 2 deps, got %d: %s", len(resp.Dependencies), string(b))
	}
	if resp.Dependencies[0].Module != "github.com/spf13/cobra" || resp.Dependencies[0].UsageCount != 2 {
		t.Errorf("first: %+v want cobra/2", resp.Dependencies[0])
	}
	if resp.Dependencies[1].Module != "golang.org/x/mod" || resp.Dependencies[1].UsageCount != 1 {
		t.Errorf("second: %+v want mod/1", resp.Dependencies[1])
	}
}

func TestListDependencies_EmptyResultSerializesAsArray(t *testing.T) {
	svc, _ := dependencies.NewService(&fakeStubAggregator{}, nil, nil)
	r := NewRegistry()
	RegisterDependenciesTool(r, svc, nil)
	raw, _ := json.Marshal(map[string]any{"repo_id": "r1", "branch": "main"})
	req := &Request{Method: "eng_list_dependencies", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	// Must serialize as {"dependencies":} not null/omitted.
	if !contains(string(b), `"dependencies":[]`) {
		t.Errorf("empty result must serialize as array, got: %s", string(b))
	}
}

func TestListDependencies_NotWired(t *testing.T) {
	r := NewRegistry()
	RegisterDependenciesTool(r, nil, nil)
	raw, _ := json.Marshal(map[string]any{"repo_id": "r1", "branch": "main"})
	req := &Request{Method: "eng_list_dependencies", Params: json.RawMessage(raw)}
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("want CodeInternalError when unwired, got %+v", rpcErr)
	}
}
