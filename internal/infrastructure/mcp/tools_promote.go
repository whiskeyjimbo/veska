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

// PromoteDeps bundles the dependencies required by the repository promotion service.
type PromoteDeps struct {
	Repos    application.RepoLister
	Git      application.GitQuerier
	Ingester PromoteIngester
	Promoter PromotePromoter
}

// PromoteIngester defines the subset of ingester functionality required for staging changes.
type PromoteIngester interface {
	Save(ctx context.Context, repoID, branch, path string, src []byte)
}

// PromotePromoter defines the subset of promoter functionality required to apply changes to the persistent graph.
type PromotePromoter interface {
	Promote(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error
}

type promoteParams struct {
	RootPath string `json:"root_path"`
	// RepoID accepts the full repository identifier or a short ID prefix.
	RepoID string `json:"repo_id"`
	Branch    string `json:"branch"`
	GitSHA    string `json:"git_sha"`
	ActorKind string `json:"actor_kind"`
	ActorID   string `json:"actor_id"`
}

type promoteResult struct {
	RepoID        string `json:"repo_id"`
	Branch        string `json:"branch"`
	GitSHA        string `json:"git_sha"`
	FilesPromoted int    `json:"files_promoted"`
}

// RegisterPromoteTool registers the eng_promote_repo tool.
func RegisterPromoteTool(r *Registry, deps PromoteDeps) {
	r.MustRegister(ToolSpec{
		Name:         "eng_promote_repo",
		Description:  "Re-stage and promote files changed in HEAD. Accepts repo_id (full or short) or root_path.",
		InputSchema:  promoteRepoInputSchema,
		Handler:      makePromoteHandler(deps),
		CLIExempt:    ExemptDeferred,
		ExemptReason: "manual promotion is a power-user knob today; CLI surface deferred until the use-case stabilises. Closest equivalent is `veska reindex` (full cold scan).",
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
		if p.RootPath == "" && p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "root_path or repo_id is required"}
		}

		if (p.ActorKind == "") != (p.ActorID == "") {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "actor_kind and actor_id must both be set or both omitted"}
		}

		repos, err := deps.Repos.ListRepos(ctx)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos: %v", err)}
		}

		var rec application.RepoRecord
		var canon string
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
			canon = rec.RootPath
		} else {
			// Canonicalise so the lookup matches the canonical form repo.Add stored.
			canon, err = filepath.EvalSymlinks(p.RootPath)
			if err != nil {

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

		if p.Branch != "" {
			branch = p.Branch
		}

		head := p.GitSHA
		if head == "" {
			h, err := deps.Git.HEAD(canon)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("git HEAD: %v", err)}
			}
			head = h
		}


		changed, err := deps.Git.ChangedFiles(canon, head)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("git ChangedFiles: %v", err)}
		}
		staged := 0
		for _, rel := range changed {
			abs := filepath.Join(canon, rel)
			src, rerr := os.ReadFile(abs)
			if rerr != nil {
				// Unreadable files are skipped during promotion.
				slog.Debug("eng_promote: skip unreadable file",
					"repo", rec.RepoID, "file", rel, "err", rerr)
				continue
			}

			deps.Ingester.Save(ctx, rec.RepoID, branch, rel, src)
			staged++
		}

		actor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
		if p.ActorKind != "" {

			a, aerr := domain.NewActor(p.ActorID, domain.ActorKind(p.ActorKind))
			if aerr != nil {
				return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid actor: %v", aerr)}
			}
			actor = *a
		}
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
