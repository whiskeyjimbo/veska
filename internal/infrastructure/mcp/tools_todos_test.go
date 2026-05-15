package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

type stubTodoQuerier struct {
	entries []ports.TodoEntry
	err     error

	gotOnlyOpen bool
}

func (s *stubTodoQuerier) FindTodos(_ context.Context, _, _ string, onlyOpen bool) ([]ports.TodoEntry, error) {
	s.gotOnlyOpen = onlyOpen
	if s.err != nil {
		return nil, s.err
	}
	return s.entries, nil
}

func dispatchTodos(t *testing.T, r *Registry, params any) (TodosResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: "eng_find_todos", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return TodosResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp TodosResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

func TestFindTodos_ReturnsOpenByDefault(t *testing.T) {
	q := &stubTodoQuerier{entries: []ports.TodoEntry{
		{FindingID: "t1", RepoID: "r", Branch: "main", FilePath: "a.go", Message: "TODO: x", State: "open"},
	}}
	r := NewRegistry()
	RegisterTodoTools(r, q)

	resp, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r", "branch": "main"})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if !q.gotOnlyOpen {
		t.Error("expected onlyOpen=true by default")
	}
	if len(resp.Todos) != 1 || resp.Todos[0].FindingID != "t1" {
		t.Errorf("unexpected todos: %+v", resp.Todos)
	}
}

func TestFindTodos_IncludeClosedFlipsOnlyOpen(t *testing.T) {
	q := &stubTodoQuerier{}
	r := NewRegistry()
	RegisterTodoTools(r, q)

	_, rpcErr := dispatchTodos(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "include_closed": true,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if q.gotOnlyOpen {
		t.Error("expected onlyOpen=false when include_closed=true")
	}
}

func TestFindTodos_RequiresParams(t *testing.T) {
	r := NewRegistry()
	RegisterTodoTools(r, &stubTodoQuerier{})

	_, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r"}) // missing branch
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestFindTodos_PropagatesError(t *testing.T) {
	q := &stubTodoQuerier{err: errors.New("disk full")}
	r := NewRegistry()
	RegisterTodoTools(r, q)

	_, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}

func TestFindTodos_RegistersOneTool(t *testing.T) {
	r := NewRegistry()
	RegisterTodoTools(r, &stubTodoQuerier{})
	got := r.Names()
	if len(got) != 1 || got[0] != "eng_find_todos" {
		t.Fatalf("expected [eng_find_todos], got %v", got)
	}
}
