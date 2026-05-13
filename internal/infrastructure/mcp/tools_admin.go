package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	application "github.com/whiskeyjimbo/engram/solov2/internal/application"
	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
)

// StatusProvider is an optional interface for eng_get_status.
// If nil, a static "ok" map is returned.
type StatusProvider interface {
	Status(ctx context.Context) (map[string]any, error)
}

// ConfigProvider is an optional interface for eng_get_config.
// If nil, a minimal static config is returned.
type ConfigProvider interface {
	Config(ctx context.Context) (map[string]any, error)
}

// RegisterAdminTools registers the 5 admin tools on r.
func RegisterAdminTools(
	r *Registry,
	repos application.RepoLister,
	status StatusProvider,
	cfg ConfigProvider,
) {
	r.MustRegister(ToolSpec{
		Name:            "eng_get_current_repo",
		Description:     "Find the indexed repo whose root contains the given cwd path.",
		IncludesStaging: true,
		Handler:         makeGetCurrentRepoHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_repos",
		Description:     "List all indexed repos registered with the daemon.",
		IncludesStaging: false,
		Handler:         makeListReposHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_repo",
		Description:     "Get a single indexed repo by its repo_id.",
		IncludesStaging: false,
		Handler:         makeGetRepoHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_status",
		Description:     "Return daemon liveness and schema version.",
		IncludesStaging: true,
		Handler:         makeGetStatusHandler(status),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_config",
		Description:     "Return effective daemon configuration (secrets redacted).",
		IncludesStaging: false,
		Handler:         makeGetConfigHandler(cfg),
	})
}

// ---------------------------------------------------------------------------
// eng_get_current_repo
// ---------------------------------------------------------------------------

type getCurrentRepoParams struct {
	CWD string `json:"cwd"`
}

func makeGetCurrentRepoHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.ActorKind, raw json.RawMessage) (any, *RPCError) {
		var p getCurrentRepoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.CWD == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "cwd is required"}
		}

		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}

		for _, rec := range all {
			if strings.HasPrefix(p.CWD, rec.RootPath) {
				return map[string]any{
					"repo":             rec,
					"included_staging": true,
					"degraded_reasons": []string{},
				}, nil
			}
		}

		return nil, &RPCError{Code: CodeInvalidParams, Message: "no indexed repo found for cwd"}
	}
}

// ---------------------------------------------------------------------------
// eng_list_repos
// ---------------------------------------------------------------------------

func makeListReposHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.ActorKind, raw json.RawMessage) (any, *RPCError) {
		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}

		return map[string]any{
			"repos":            all,
			"included_staging": false,
			"degraded_reasons": []string{},
		}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_get_repo
// ---------------------------------------------------------------------------

type getRepoParams struct {
	RepoID string `json:"repo_id"`
}

func makeGetRepoHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.ActorKind, raw json.RawMessage) (any, *RPCError) {
		var p getRepoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}

		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}

		for _, rec := range all {
			if rec.RepoID == p.RepoID {
				return map[string]any{
					"repo":             rec,
					"included_staging": false,
					"degraded_reasons": []string{},
				}, nil
			}
		}

		return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
	}
}

// ---------------------------------------------------------------------------
// eng_get_status
// ---------------------------------------------------------------------------

func makeGetStatusHandler(sp StatusProvider) ToolHandler {
	return func(ctx context.Context, _ domain.ActorKind, raw json.RawMessage) (any, *RPCError) {
		if sp != nil {
			m, err := sp.Status(ctx)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("status failed: %v", err)}
			}
			return m, nil
		}

		return map[string]any{
			"status":           "ok",
			"schema_version":   1,
			"included_staging": true,
			"degraded_reasons": []string{},
		}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_get_config
// ---------------------------------------------------------------------------

func makeGetConfigHandler(cp ConfigProvider) ToolHandler {
	return func(ctx context.Context, _ domain.ActorKind, raw json.RawMessage) (any, *RPCError) {
		if cp != nil {
			m, err := cp.Config(ctx)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("config failed: %v", err)}
			}
			return m, nil
		}

		return map[string]any{
			"engram_home":      config.DefaultVectorDir(),
			"schema_version":   1,
			"included_staging": false,
			"degraded_reasons": []string{},
		}, nil
	}
}
