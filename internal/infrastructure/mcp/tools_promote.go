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
// post-commit promotion was effectively dead .
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
	// RepoID accepts the full repo_id or a 12-char short_id ,
	// matching every other repo-scoped tool. Either RepoID or RootPath is
	// sufficient; when both are passed, RepoID wins.
	RepoID string `json:"repo_id"`
	// Optional overrides . Pre-validator the handler silently
	// dropped these; agents calling eng_promote_repo with attribution had no
	// way to learn that. They are now first-class:
	//   - Branch overrides the repo's active_branch when non-empty.
	//   - GitSHA pins the commit to promote at; defaults to git HEAD.
	//   - ActorKind + ActorID stamp attribution on the promotion; default is
	//     the system actor ('service:veska'). They must be supplied together
	//     or both omitted.
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

// RegisterPromoteTool wires eng_promote. Nil deps are tolerated: the handler
// returns a clear error rather than panicking, so a misconfigured wiring
// degrades to "promote is unavailable" without taking the daemon down.
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
		// actor_kind / actor_id must be supplied together . The
		// schema can't express "all-or-none", so validate here.
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
			// solov2-65bk: resolve by repo_id (full or short prefix) — parity
			// with every other repo-scoped tool.
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
				// Fall back to an absolute path — repo.Add does the same when
				// EvalSymlinks fails on a missing directory.
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
		// solov2-cyww: caller-supplied branch override (e.g. an agent
		// re-promoting a non-active branch) takes precedence over the
		// repo-record default.
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
		if p.ActorKind != "" {
			// NewActor enforces the kind enum (human/agent/system) and
			// rejects an empty id, so an invalid actor_kind surfaces here
			// as CodeInvalidParams rather than silently degrading to the
			// system default .
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
