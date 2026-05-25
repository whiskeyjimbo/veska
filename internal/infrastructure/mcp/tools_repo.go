package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RepoRegistrar registers and deregisters tracked repositories. It is the
// port consumed by eng_add_repo / eng_remove_repo; the composition root wires
// an adapter over internal/repo so the MCP layer stays decoupled from it.
//
// Add registers root_path and returns the repo_id; registration is expected
// to return before any cold scan completes (the scan runs asynchronously via
// the daemon's queue/watcher). Remove drops the repo's rows in one
// transaction (CASCADE removes nodes/edges).
type RepoRegistrar interface {
	AddRepo(ctx context.Context, rootPath string) (repoID string, existed bool, err error)
	RemoveRepo(ctx context.Context, repoID string) error
}

// RegisterRepoTools registers eng_add_repo and eng_remove_repo on r.
// reg is the RepoRegistrar adapter; when nil, both tools return an
// internal error so a misconfigured daemon fails loudly rather than silently.
func RegisterRepoTools(r *Registry, reg RepoRegistrar) {
	r.MustRegister(ToolSpec{
		Name:            "eng_add_repo",
		Description:     "Register a new repo path; the daemon kicks off a cold scan in the background.",
		IncludesStaging: false,
		InputSchema:     addRepoInputSchema,
		Handler:         makeAddRepoHandler(reg),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_remove_repo",
		Description:     "Unregister a repo and drop all of its rows in one transaction.",
		IncludesStaging: false,
		InputSchema:     removeRepoInputSchema,
		Handler:         makeRemoveRepoHandler(reg),
	})
}

// ---------------------------------------------------------------------------
// eng_add_repo
// ---------------------------------------------------------------------------

type addRepoParams struct {
	RootPath string `json:"root_path"`
}

func makeAddRepoHandler(reg RepoRegistrar) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p addRepoParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("root_path", p.RootPath); rpcErr != nil {
			return nil, rpcErr
		}
		if reg == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "repo registrar unavailable"}
		}

		// Add returns once the repo row is inserted and hooks are installed;
		// the cold scan is driven asynchronously by the daemon's queue/watcher.
		// already_registered=true means the row already existed and no scan was
		// dispatched (solov2-khjd) — the CLI uses this to print an idempotency
		// message instead of a misleading 'added'.
		id, existed, err := reg.AddRepo(ctx, p.RootPath)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("add repo: %v", err)}
		}

		return map[string]any{
			"repo_id":            id,
			"root_path":          p.RootPath,
			"scan_pending":       !existed,
			"already_registered": existed,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_remove_repo
// ---------------------------------------------------------------------------

type removeRepoParams struct {
	RepoID string `json:"repo_id"`
}

func makeRemoveRepoHandler(reg RepoRegistrar) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p removeRepoParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
		}
		if reg == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "repo registrar unavailable"}
		}

		if err := reg.RemoveRepo(ctx, p.RepoID); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("remove repo: %v", err)}
		}

		return map[string]any{
			"repo_id": p.RepoID,
			"removed": true,
		}, nil
	}
}
