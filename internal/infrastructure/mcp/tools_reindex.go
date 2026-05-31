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

// ---------------------------------------------------------------------------
// eng_reindex_repo
//
// `veska reindex` previously refused to run while the daemon was up because it
// opened SQLite directly and would race the daemon's embedder worker for the
// write lock . Telling a junior user to stop the daemon, reindex,
// then restart kills their editor's MCP connection mid-task.
//
// This tool moves the reindex *into* the daemon — same cold-scan reparser the
// add-repo path uses (solov2-0z1.3) — so `veska reindex` dispatches via MCP
// when a daemon is running and the editor stays connected throughout.
//
// Inputs:
//
//	repo_id    — full repo_id or short_id prefix (mirrors eng_promote_repo).
//	root_path  — absolute filesystem path; canonicalised via EvalSymlinks.
//
// One of the two is required. When both are passed, repo_id wins.
//
// The handler runs the cold-scan synchronously under the request context and
// returns when the scan completes, so the CLI gets a deterministic "done"
// signal instead of having to poll eng_get_status's scans_in_flight. The
// reparser emits the standard "cold scan: starting" / "cold scan: complete"
// log pair the add-repo path already produces, keeping daemon.log consistent.
// ---------------------------------------------------------------------------

// ReindexDeps bundles the collaborators eng_reindex_repo needs. Repos resolves
// repo_id / root_path to an application.RepoRecord; Reparser runs the actual
// cold-scan. The composition root wires both from the same singletons the
// daemon already builds for the add-repo cold-scan dispatch.
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

// RegisterReindexTool wires eng_reindex_repo. Nil deps degrade to a clear
// internal-error response rather than a panic — the daemon stays up even if
// the wiring is incomplete.
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
		// Normalise the record before passing to the reparser so the cold-scan
		// path observes the same default branch the resync path does.
		toScan := rec
		toScan.ActiveBranch = branch

		if err := deps.Reparser(ctx, toScan); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, &RPCError{Code: CodeInternalError, Message: "reindex cancelled"}
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
