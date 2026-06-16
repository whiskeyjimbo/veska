package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// RepoRegistrar registers and deregisters tracked repositories. It is the
// port consumed by eng_add_repo / eng_remove_repo; the composition root wires
// an adapter over internal/repo so the MCP layer stays decoupled from it.
// Add registers root_path and returns the repo_id; registration is expected
// to return before any cold scan completes (the scan runs asynchronously via
// the daemon's queue/watcher). Remove drops the repo's rows in one
// transaction (CASCADE removes nodes/edges).
type RepoRegistrar interface {
	AddRepo(ctx context.Context, rootPath string) (repoID string, existed bool, err error)
	RemoveRepo(ctx context.Context, repoID string) error
	// SetAlias binds name to repoID. force=true overwrites an existing
	// binding to a different repo; without it, the conflict surfaces as
	// an ErrAliasExists.
	SetAlias(ctx context.Context, name, repoID string, force bool) error
	// RemoveAlias drops the alias name. Unknown names return an error so
	// a typo is loud.
	RemoveAlias(ctx context.Context, name string) error
}

// RegisterRepoTools registers eng_add_repo / eng_remove_repo /
// eng_set_repo_alias / eng_remove_repo_alias on r. reg is the
// RepoRegistrar adapter; when nil, the tools return an internal error so
// a misconfigured daemon fails loudly rather than silently.
func RegisterRepoTools(r *Registry, reg RepoRegistrar, repos application.RepoLister) {
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
	r.MustRegister(ToolSpec{
		Name:            "eng_set_repo_alias",
		Description:     "Bind a human-friendly alias to a repo. Resolves repo_id via the usual progression (full id, short_id, existing alias, prefix). force=true overwrites an existing alias on a different repo.",
		IncludesStaging: false,
		InputSchema:     setRepoAliasInputSchema,
		Handler:         makeSetRepoAliasHandler(reg, repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_remove_repo_alias",
		Description:     "Remove a user-defined alias by name.",
		IncludesStaging: false,
		InputSchema:     removeRepoAliasInputSchema,
		Handler:         makeRemoveRepoAliasHandler(reg),
	})
}

// eng_add_repo

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
		// dispatched — the CLI uses this to print an idempotency
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

// eng_remove_repo

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

// eng_set_repo_alias

type setRepoAliasParams struct {
	Name   string `json:"name"`
	RepoID string `json:"repo_id"`
	Force  bool   `json:"force"`
}

func makeSetRepoAliasHandler(reg RepoRegistrar, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p setRepoAliasParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("name", p.Name); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
		}
		if reg == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "repo registrar unavailable"}
		}

		canonical, rpcErr := resolveRepoID(ctx, repos, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}

		if err := reg.SetAlias(ctx, p.Name, canonical, p.Force); err != nil {
			switch {
			case errors.Is(err, repo.ErrAliasExists):
				return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("%v (pass force=true to overwrite)", err)}
			case errors.Is(err, repo.ErrAliasInvalid):
				return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
			default:
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("set alias: %v", err)}
			}
		}
		return map[string]any{
			"name":    p.Name,
			"repo_id": canonical,
		}, nil
	}
}

// eng_remove_repo_alias

type removeRepoAliasParams struct {
	Name string `json:"name"`
}

func makeRemoveRepoAliasHandler(reg RepoRegistrar) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p removeRepoAliasParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("name", p.Name); rpcErr != nil {
			return nil, rpcErr
		}
		if reg == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "repo registrar unavailable"}
		}
		if err := reg.RemoveAlias(ctx, p.Name); err != nil {
			if errors.Is(err, repo.ErrAliasNotFound) {
				return nil, &RPCError{Code: CodeNotFound, Message: err.Error()}
			}
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("remove alias: %v", err)}
		}
		return map[string]any{
			"name":    p.Name,
			"removed": true,
		}, nil
	}
}
