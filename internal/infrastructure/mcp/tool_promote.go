package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ---------------------------------------------------------------------------
// eng_promote
//
// Called by the post-commit git hook to drive a real promotion after a commit.
// The previous wire protocol — a bare {"cmd":"promote"} payload — was rejected
// by the JSON-RPC listener with method-not-found and silently swallowed, so
// post-commit promotion was effectively dead (solov2-3vv).
//
// Inputs:
//
//	root_path  — absolute working-tree path of the just-committed repo.
//
// Action:
//  1. Resolve root_path → registered RepoRecord via the lister.
//  2. Read HEAD from git.
//  3. Read the files changed in the HEAD commit and Ingester.Save each one,
//     so the staging area is populated even when fsnotify missed the events
//     (the hook is the second-chance gate, not just a notification).
//  4. Promoter.Promote(repoID, branch, HEAD, system actor) flushes staging.
//
// Idempotent and bounded — files deleted in the commit are skipped, parse
// errors are logged but don't abort the promotion.
// ---------------------------------------------------------------------------

// PromoteDeps bundles the collaborators eng_promote needs. The wire layer
// constructs these from the same singletons the daemon already builds —
// repoLister, GitQuerier, Ingester, Promoter — so eng_promote and the
// startup-resync / cold-scan paths share one source of truth per dependency.
type PromoteDeps struct {
	Repos    application.RepoLister
	Git      application.GitQuerier
	Ingester PromoteIngester
	Promoter PromotePromoter
}

// PromoteIngester is the narrow surface eng_promote needs from the Ingester
// — kept narrow so future Ingester refactors don't ripple here.
type PromoteIngester interface {
	Save(ctx context.Context, repoID, branch, path string, src []byte)
}

// PromotePromoter is the narrow surface eng_promote needs from the Promoter.
type PromotePromoter interface {
	Promote(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error
}

type promoteParams struct {
	RootPath string `json:"root_path"`
}

type promoteResult struct {
	RepoID        string `json:"repo_id"`
	Branch        string `json:"branch"`
	GitSHA        string `json:"git_sha"`
	FilesPromoted int    `json:"files_promoted"`
}

// RegisterPromoteTool wires eng_promote. Nil deps are tolerated: the handler
// returns a clear error rather than panicking, so a misconfigured wiring
// degrades to "promote is unavailable" without taking the daemon down.
func RegisterPromoteTool(r *Registry, deps PromoteDeps) {
	r.MustRegister(ToolSpec{
		Name:        "eng_promote_repo",
		Description: "Re-stage and promote files changed in HEAD for the repo at root_path.",
		Handler:     makePromoteHandler(deps),
	})
}

func makePromoteHandler(deps PromoteDeps) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if deps.Repos == nil || deps.Git == nil || deps.Ingester == nil || deps.Promoter == nil {
			return nil, &RPCError{Code: CodeInternalError, Message: "eng_promote: handler not fully wired"}
		}
		var p promoteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RootPath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "root_path is required"}
		}
		// Canonicalise so the lookup matches the canonical form repo.Add stored.
		canon, err := filepath.EvalSymlinks(p.RootPath)
		if err != nil {
			// Fall back to an absolute path — repo.Add does the same when
			// EvalSymlinks fails on a missing directory.
			if abs, aerr := filepath.Abs(p.RootPath); aerr == nil {
				canon = abs
			} else {
				canon = p.RootPath
			}
		}

		repos, err := deps.Repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos: %v", err)}
		}
		var rec application.RepoRecord
		for _, r := range repos {
			if r.RootPath == canon {
				rec = r
				break
			}
		}
		if rec.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo not registered for root %q", canon)}
		}
		branch := rec.ActiveBranch
		if branch == "" {
			branch = "main"
		}

		head, err := deps.Git.HEAD(canon)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("git HEAD: %v", err)}
		}

		// Re-stage files changed in the HEAD commit. Treating the hook as the
		// second-chance gate (rather than trusting fsnotify alone) is what
		// makes commit-driven updates land deterministically.
		changed, err := deps.Git.ChangedFiles(canon, head)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("git ChangedFiles: %v", err)}
		}
		staged := 0
		for _, rel := range changed {
			abs := filepath.Join(canon, rel)
			src, rerr := os.ReadFile(abs)
			if rerr != nil {
				// File deleted in this commit or unreadable — skip; Promoter
				// will not produce nodes for it, which is the correct outcome.
				slog.Debug("eng_promote: skip unreadable file",
					"repo", rec.RepoID, "file", rel, "err", rerr)
				continue
			}
			deps.Ingester.Save(ctx, rec.RepoID, branch, abs, src)
			staged++
		}

		actor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
		if err := deps.Promoter.Promote(ctx, rec.RepoID, branch, head, actor); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("promote: %v", err)}
		}

		return promoteResult{
			RepoID:        rec.RepoID,
			Branch:        branch,
			GitSHA:        head,
			FilesPromoted: staged,
		}, nil
	}
}
