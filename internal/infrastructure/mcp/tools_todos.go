package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TodoDTO is the snake_case wire shape for a single TODO/FIXME marker. The
// mcp layer owns its serialization rather than emitting the raw
// ports.TodoEntry, whose PascalCase Go field names would otherwise leak into
// the JSON-RPC response and break the snake_case surface contract (solov2-unem).
type TodoDTO struct {
	FindingID string `json:"finding_id"`
	RepoID    string `json:"repo_id"`
	Branch    string `json:"branch"`
	FilePath  string `json:"file_path"`
	Message   string `json:"message"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
}

// TodosResponse is the envelope returned by eng_find_todos.
type TodosResponse struct {
	Todos []TodoDTO `json:"todos"`
}

func todosToDTO(in []ports.TodoEntry) []TodoDTO {
	out := make([]TodoDTO, 0, len(in))
	for _, e := range in {
		out = append(out, TodoDTO{
			FindingID: e.FindingID,
			RepoID:    e.RepoID,
			Branch:    e.Branch,
			FilePath:  e.FilePath,
			Message:   e.Message,
			State:     e.State,
			CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// RegisterTodoTools registers eng_find_todos on r.
// querier is required.
func RegisterTodoTools(r *Registry, querier ports.TodoQuerier, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_todos",
		Description:     "List parser-detected TODO/FIXME markers for the given (repo, branch).",
		IncludesStaging: false,
		Handler:         makeFindTodosHandler(querier, repos),
	})
}

type findTodosParams struct {
	RepoID        string `json:"repo_id"`
	Branch        string `json:"branch"`
	IncludeClosed bool   `json:"include_closed,omitempty"`
}

func makeFindTodosHandler(querier ports.TodoQuerier, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findTodosParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
		}
		repoID, rpcErr := resolveRepoID(ctx, repos, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}
		entries, err := querier.FindTodos(ctx, p.RepoID, p.Branch, !p.IncludeClosed)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find todos: %v", err)}
		}
		return TodosResponse{Todos: todosToDTO(entries)}, nil
	}
}
