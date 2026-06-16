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

// TodoDTO is the snake_case wire shape for a single TODO/FIXME marker. The
// mcp layer owns its serialization rather than emitting the raw
// ports.TodoEntry, whose PascalCase Go field names would otherwise leak into
// the JSON-RPC response and break the snake_case surface contract.
type TodoDTO struct {
	FindingID string `json:"finding_id"`
	RepoID    string `json:"repo_id"`
	Branch    string `json:"branch"`
	FilePath  string `json:"file_path"`
	Message   string `json:"message"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
}

// TodosResponse is the envelope returned by eng_find_todos. DegradedReasons
// is always emitted (as when nothing is degraded) so the wire shape
// matches every other query tool per the README's "Conventions across the
// tool surface" contract.
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

// relativizeToRoot is the value-typed twin of relativizeFindingPath used by
// list_findings: TodoDTO.FilePath is a plain string (todos always have a
// path), but eng_find_todos and eng_list_findings must agree on the wire
// convention — repo-relative.
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

// RegisterTodoTools registers eng_find_todos on r.
// querier is required.
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
		// shim-injected cwd resolves repo_id when omitted.
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
		// emit repo-relative file_path so eng_find_todos and
		// eng_list_findings agree on shape per.
		var root string
		if repos != nil {
			if r, ok := repoRoot(ctx, repos, p.RepoID); ok {
				root = r
			}
		}
		// TODO scanning is post-promotion (it indexes the last
		// committed tree). When the result is empty but the working tree
		// has uncommitted changes, surface a degraded_reason so callers can
		// say "commit first" instead of "no TODOs". Non-empty results stay
		// degraded-free — staged TODOs from prior commits remain valid.
		degraded := []string{}
		if len(entries) == 0 && root != "" && gitinfra.WorkingTreeHasUncommittedChanges(ctx, root) {
			degraded = append(degraded, "todos_are_post_promotion")
		}
		return TodosResponse{Todos: todosToDTO(entries, root), DegradedReasons: degraded}, nil
	}
}
