package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RegisterContextPackTool registers eng_get_context_pack. asm and repoRoot
// are required; when either is nil the tool is still registered but
// returns InternalError on every call, keeping the registry uniform
// across composition roots that have not wired the context-pack service.
func RegisterContextPackTool(r *Registry, asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_context_pack",
		Description: "Return a token-bounded JSON bundle of relevant nodes, recent commits, open findings and tasks for a symbol or a task.",
		InputSchema: contextPackInputSchema,
		Handler:     makeContextPackHandler(asm, repoRoot, repos),
	})
}

type contextPackParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	NodeID string `json:"node_id,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

func makeContextPackHandler(asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if asm == nil || repoRoot == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "context pack is not wired (assembler or repoRoot missing)",
			}
		}
		var p contextPackParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// Exactly one of node_id / symbol / task_id is required (solov2-z81b).
		anchorCount := 0
		if p.NodeID != "" {
			anchorCount++
		}
		if p.Symbol != "" {
			anchorCount++
		}
		if p.TaskID != "" {
			anchorCount++
		}
		if anchorCount != 1 {
			return nil, &RPCError{
				Code:    CodeInvalidParams,
				Message: "exactly one of node_id, symbol or task_id is required",
			}
		}
		// solov2-ktz0: shim-injected cwd resolves repo_id when omitted.
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
		root, err := repoRoot(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
		}
		if root == "" {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
		}

		var pack contextpack.Pack
		switch {
		case p.NodeID != "":
			pack, err = asm.ForNode(ctx, p.RepoID, p.Branch, root, p.NodeID)
		case p.Symbol != "":
			pack, err = asm.ForSymbol(ctx, p.RepoID, p.Branch, root, p.Symbol)
		default:
			pack, err = asm.ForTask(ctx, p.RepoID, p.Branch, root, p.TaskID)
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("context pack: %v", err)}
		}
		return pack, nil
	}
}
