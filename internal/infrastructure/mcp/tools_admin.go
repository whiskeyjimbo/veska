// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// StatusProvider is an optional interface to supply dynamic status metrics for eng_get_status.
type StatusProvider interface {
	Status(ctx context.Context) (map[string]any, error)
}

// ConfigProvider is an optional interface to supply effective configuration values for eng_get_config.
type ConfigProvider interface {
	Config(ctx context.Context) (map[string]any, error)
}

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
		InputSchema:     getCurrentRepoInputSchema,
		Handler:         makeGetCurrentRepoHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_list_repos",
		Description:     "List all indexed repos registered with the daemon.",
		IncludesStaging: false,
		InputSchema:     listReposInputSchema,
		Handler:         makeListReposHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_repo",
		Description:     "Get a single indexed repo by its repo_id.",
		IncludesStaging: false,
		InputSchema:     getRepoInputSchema,
		Handler:         makeGetRepoHandler(repos),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_status",
		Description:     "Return daemon liveness and schema version.",
		IncludesStaging: true,
		InputSchema:     getStatusInputSchema,
		Handler:         makeGetStatusHandler(status),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_config",
		Description:     "Return effective daemon configuration (secrets redacted).",
		IncludesStaging: false,
		InputSchema:     getConfigInputSchema,
		Handler:         makeGetConfigHandler(cfg),
	})
}

type getCurrentRepoParams struct {
	CWD string `json:"cwd"`
}

func makeGetCurrentRepoHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getCurrentRepoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}

		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}

		// When cwd is omitted, we resolve to the sole registered repo as a fallback for simple single-repo environments.
		if p.CWD == "" {
			// We exclude synthetic external dependency repos (prefixed with 'ext:') to ensure fallback resolution targets the primary user-registered repo.
			var sole *application.RepoRecord
			ambiguous := false
			for i := range all {
				if strings.HasPrefix(all[i].RepoID, "ext:") {
					continue
				}
				if sole != nil {
					ambiguous = true
					break
				}
				sole = &all[i]
			}
			if sole != nil && !ambiguous {
				return map[string]any{
					"repo":             decorateRepo(*sole),
					"included_staging": true,
					"degraded_reasons": []string{"defaulted_to_sole_repo"},
				}, nil
			}
			return nil, &RPCError{
				Code:    CodeInvalidParams,
				Message: `cwd is required when more than one repo is registered (pass {"cwd": "/abs/path/to/checkout"}; with a single registered repo, cwd may be omitted)`,
			}
		}

		for _, rec := range all {
			if strings.HasPrefix(p.CWD, rec.RootPath) {
				return map[string]any{
					"repo":             decorateRepo(rec),
					"included_staging": true,
					"degraded_reasons": []string{},
				}, nil
			}
		}

		return nil, &RPCError{Code: CodeInvalidParams, Message: "no indexed repo found for cwd"}
	}
}

// RepoView decorates the raw RepoRecord with a derived status field to distinguish promoted, unindexed, and missing repositories.
type RepoView struct {
	RepoID          string   `json:"repo_id"`
	ShortID         string   `json:"short_id"`
	RootPath        string   `json:"root_path"`
	ActiveBranch    string   `json:"active_branch"`
	LastPromotedSHA string   `json:"last_promoted_sha"`
	Status          string   `json:"status"`
	Kind            string   `json:"kind"`
	Aliases         []string `json:"aliases"`
}

// ShortRepoIDLen specifies the character length used to slice the unique repository hash into a readable shorthand identifier.
const ShortRepoIDLen = 12

func ShortRepoID(id string) string {
	if len(id) <= ShortRepoIDLen {
		return id
	}
	return id[:ShortRepoIDLen]
}

func decorateRepo(r application.RepoRecord) RepoView {
	status := "promoted"
	if r.LastPromotedSHA == "" {
		status = "unindexed"
	}
	if r.RootPath != "" {
		if _, err := os.Stat(r.RootPath); errors.Is(err, fs.ErrNotExist) {
			status = "missing"
		}
	}
	kind := r.Kind
	if kind == "" {
		// Empty kind fields are default-valued to 'tracked' to prevent blank outputs on older registration rows.
		kind = "tracked"
	}
	aliases := r.Aliases
	if aliases == nil {
		// Nil aliases slices are explicitly converted to empty slices to ensure they serialize as empty JSON arrays rather than null.
		aliases = []string{}
	}
	return RepoView{
		RepoID:          r.RepoID,
		ShortID:         ShortRepoID(r.RepoID),
		RootPath:        r.RootPath,
		ActiveBranch:    r.ActiveBranch,
		LastPromotedSHA: r.LastPromotedSHA,
		Status:          status,
		Kind:            kind,
		Aliases:         aliases,
	}
}

func decorateRepos(in []application.RepoRecord) []RepoView {
	out := make([]RepoView, 0, len(in))
	for _, r := range in {
		out = append(out, decorateRepo(r))
	}
	return out
}

// listReposParams supports including synthetic dependency repositories via IncludeVendored.
type listReposParams struct {
	IncludeVendored bool `json:"include_vendored"`
}

func makeListReposHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p listReposParams
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &p)
		}
		all, err := repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
		}
		if !p.IncludeVendored {
			filtered := all[:0]
			for _, r := range all {
				if strings.HasPrefix(r.RepoID, "ext:") {
					continue
				}
				filtered = append(filtered, r)
			}
			all = filtered
		}

		return map[string]any{
			"repos":            decorateRepos(all),
			"included_staging": false,
			"degraded_reasons": []string{},
		}, nil
	}
}

type getRepoParams struct {
	RepoID string `json:"repo_id"`
}

func makeGetRepoHandler(repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
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
			if rec.RepoID == p.RepoID || ShortRepoID(rec.RepoID) == p.RepoID {
				return map[string]any{
					"repo":             decorateRepo(rec),
					"included_staging": false,
					"degraded_reasons": []string{},
				}, nil
			}
		}

		return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
	}
}

func makeGetStatusHandler(sp StatusProvider) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
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

func makeGetConfigHandler(cp ConfigProvider) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if cp != nil {
			m, err := cp.Config(ctx)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("config failed: %v", err)}
			}
			return m, nil
		}

		return map[string]any{
			"veska_home":            config.DefaultVectorDir(),
			"config_schema_version": 1,
			"included_staging":      false,
			"degraded_reasons":      []string{},
		}, nil
	}
}
