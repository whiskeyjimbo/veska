// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// TodoDTO is the external wire shape for a single TODO/FIXME marker.
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
	Todos           []TodoDTO `json:"todos"`
	DegradedReasons []string  `json:"degraded_reasons"`
}

func todosToDTO(in []ports.TodoEntry, repoRoot string) []TodoDTO {
	out := make([]TodoDTO, 0, len(in))
	for _, e := range in {
		out = append(out, TodoDTO{
			FindingID: e.FindingID,
			RepoID:    e.RepoID,
			Branch:    e.Branch,
			FilePath:  relativizeToRoot(e.FilePath, repoRoot),
			Message:   e.Message,
			State:     e.State,
			CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// relativizeToRoot normalizes a file path to be relative to the repository root.
func relativizeToRoot(path, root string) string {
	if path == "" || root == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// RegisterTodoTools registers TODO query tools in the registry.
func RegisterTodoTools(r *Registry, querier ports.TodoQuerier, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_todos",
		Description:     "List parser-detected TODO/FIXME markers for the given (repo, branch).",
		IncludesStaging: false,
		InputSchema:     findTodosInputSchema,
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

		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
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

		var root string
		if repos != nil {
			if r, ok := repoRoot(ctx, repos, p.RepoID); ok {
				root = r
			}
		}
		// Empty results when the working tree has uncommitted changes return a degraded reason to warn that TODOs are only scanned post-promotion.
		degraded := []string{}
		if len(entries) == 0 && root != "" && gitinfra.WorkingTreeHasUncommittedChanges(ctx, root) {
			degraded = append(degraded, "todos_are_post_promotion")
		}
		return TodosResponse{Todos: todosToDTO(entries, root), DegradedReasons: degraded}, nil
	}
}
