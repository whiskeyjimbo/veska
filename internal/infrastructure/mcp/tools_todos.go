package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TodosResponse is the envelope returned by eng_find_todos.
type TodosResponse struct {
	Todos []ports.TodoEntry `json:"todos"`
}

// RegisterTodoTools registers eng_find_todos on r.
// querier is required.
func RegisterTodoTools(r *Registry, querier ports.TodoQuerier) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_todos",
		Description:     "List parser-detected TODO/FIXME markers for the given (repo, branch).",
		IncludesStaging: false,
		Handler:         makeFindTodosHandler(querier),
	})
}

type findTodosParams struct {
	RepoID        string `json:"repo_id"`
	Branch        string `json:"branch"`
	IncludeClosed bool   `json:"include_closed,omitempty"`
}

func makeFindTodosHandler(querier ports.TodoQuerier) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findTodosParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}
		entries, err := querier.FindTodos(ctx, p.RepoID, p.Branch, !p.IncludeClosed)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find todos: %v", err)}
		}
		if entries == nil {
			entries = []ports.TodoEntry{}
		}
		return TodosResponse{Todos: entries}, nil
	}
}
