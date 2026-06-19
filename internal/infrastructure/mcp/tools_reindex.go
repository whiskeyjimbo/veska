// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ReindexDeps bundles the repository lister and reparser dependencies needed to run in-daemon reindexing.
type ReindexDeps struct {
	Repos    application.RepoLister
	Reparser func(ctx context.Context, rec application.RepoRecord) error
}

type reindexParams struct {
	RepoID   string `json:"repo_id"`
	RootPath string `json:"root_path"`
}

type reindexResult struct {
	RepoID   string `json:"repo_id"`
	RootPath string `json:"root_path"`
	Branch   string `json:"branch"`
	Status   string `json:"status"`
}

// RegisterReindexTool registers the eng_reindex_repo tool.
func RegisterReindexTool(r *Registry, deps ReindexDeps) {
	r.MustRegister(ToolSpec{
		Name:        "eng_reindex_repo",
		Description: "Force a full cold-scan reparse of a registered repo. Accepts repo_id (full or short) or root_path. Runs in-daemon so callers do not need to stop the service.",
		InputSchema: reindexRepoInputSchema,
		Handler:     makeReindexHandler(deps),
	})
}

func makeReindexHandler(deps ReindexDeps) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if deps.Repos == nil || deps.Reparser == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "eng_reindex_repo: handler not fully wired"}
		}
		var p reindexParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RepoID == "" && p.RootPath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id or root_path is required"}
		}

		repos, err := deps.Repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos: %v", err)}
		}

		var rec application.RepoRecord
		if p.RepoID != "" {
			for _, r := range repos {
				if r.RepoID == p.RepoID || ShortRepoID(r.RepoID) == p.RepoID {
					rec = r
					break
				}
			}
			if rec.RepoID == "" {
				return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
			}
		} else {
			canon, cerr := filepath.EvalSymlinks(p.RootPath)
			if cerr != nil {
				if abs, aerr := filepath.Abs(p.RootPath); aerr == nil {
					canon = abs
				} else {
					canon = p.RootPath
				}
			}
			for _, r := range repos {
				if r.RootPath == canon {
					rec = r
					break
				}
			}
			if rec.RepoID == "" {
				return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not registered for root %q", canon)}
			}
		}

		branch := rec.ActiveBranch
		if branch == "" {
			branch = "main"
		}

		toScan := rec
		toScan.ActiveBranch = branch

		if err := deps.Reparser(ctx, toScan); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, &RPCError{Code: CodeInternalError, Message: "reindex canceled"}
			}
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("reindex: %v", err)}
		}

		return reindexResult{
			RepoID:   rec.RepoID,
			RootPath: rec.RootPath,
			Branch:   branch,
			Status:   "complete",
		}, nil
	}
}
