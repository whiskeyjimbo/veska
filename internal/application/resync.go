package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// isMissingRoot reports whether the resync error is the expected
// "registered repo whose working tree is gone" case. The git CLI
// emits "cannot change to '<path>': No such file or directory" via
// rev-parse when the root is missing; matching on that substring
// keeps the check decoupled from the exact wrapped-error chain.
func isMissingRoot(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "No such file or directory") || strings.Contains(s, "no such file or directory")
}

// RepoRecord is the minimal view of a repos row needed for resync.
type RepoRecord struct {
	RepoID          string
	RootPath        string // absolute path to the git working tree
	ActiveBranch    string
	LastPromotedSHA string // empty = never promoted
	Kind            string // "tracked" (default) or "ephemeral"
	// Aliases are user-defined human-friendly names for this repo
	// MCP resolvers use them for repo_id lookup; the
	// MCP eng_list_repos response surfaces them per repo.
	Aliases []string
}

// RepoLister lists all registered repos.
type RepoLister interface {
	ListRepos(ctx context.Context) ([]RepoRecord, error)
}

// GitQuerier is the port for git history queries needed by resync.
// Implementations call os/exec git commands.
type GitQuerier interface {
	// HEAD returns the current HEAD SHA for the repo at rootPath.
	HEAD(rootPath string) (string, error)
	// IsAncestor returns true if sha is reachable from HEAD (i.e. sha is an ancestor).
	IsAncestor(rootPath, sha, head string) (bool, error)
	// CommitsSince returns the list of commit SHAs from sha.HEAD in oldest-first order,
	// i.e. the output of `git log <sha>.HEAD --reverse --format=%H`.
	CommitsSince(rootPath, sha, head string) ([]string, error)
	// ChangedFiles returns the list of files changed in the given commit SHA.
	ChangedFiles(rootPath, sha string) ([]string, error)
	// ReadFileAtCommit returns the contents of filePath at the given commit SHA.
	ReadFileAtCommit(rootPath, sha, filePath string) ([]byte, error)
}

// ErrPromotionDivergent is returned when last_promoted_sha is not reachable
// from HEAD (force-push or history rewrite).
type ErrPromotionDivergent struct {
	RepoID string
	SHA    string
}

func (e *ErrPromotionDivergent) Error() string {
	return fmt.Sprintf("veska: promotion divergent for repo %q: SHA %q is not reachable from HEAD", e.RepoID, e.SHA)
}

// StartupResync runs the startup resync for all registered repos.
// It is called before the daemon accepts connections.
// During resync, IsSyncing returns true.
type StartupResync struct {
	repos    RepoLister
	git      GitQuerier
	save     saveFunc
	promote  promoteFunc
	reparser func(ctx context.Context, repo RepoRecord) error

	// br is the optional staging-vs-HEAD branch reconciler.
	// When set, each repo's working-tree branch is reconciled against
	// repos.active_branch before any parse/replay. nil = skip (back-compat).
	br *BranchReconciler

	syncing atomic.Bool
}

// StartupResyncOption configures optional StartupResync behaviour.
type StartupResyncOption func(*StartupResync)

// WithBranchReconciler wires the staging-vs-HEAD branch reconciler (
// §5.2) into the startup-resync path. A nil reconciler is ignored, leaving the
// branch check disabled (back-compat). When set, resyncRepo reconciles the
// working-tree branch against repos.active_branch before any replay runs, and
// the resolved branch flows into the per-commit save/promote key.
func WithBranchReconciler(br *BranchReconciler) StartupResyncOption {
	return func(s *StartupResync) {
		if br != nil {
			s.br = br
		}
	}
}

// NewStartupResync constructs a StartupResync wired to the provided dependencies.
// save and promote are the narrow seams over Ingester.Save and Promoter.Promote
// (callers pass ingester.Save / promoter.Promote); taking the seams rather than
// the concrete types matches the cold-scan path and keeps the test surface
// small. reparser is called for full-reparse paths (never-promoted or divergent
// SHA).
func NewStartupResync(
	repos RepoLister,
	git GitQuerier,
	save saveFunc,
	promote promoteFunc,
	reparser func(ctx context.Context, repo RepoRecord) error,
	opts ...StartupResyncOption,
) (*StartupResync, error) {
	if repos == nil || git == nil || save == nil || promote == nil || reparser == nil {
		return nil, fmt.Errorf("application.NewStartupResync: nil dependency: %w", ErrMissingDependency)
	}
	sr := &StartupResync{
		repos:    repos,
		git:      git,
		save:     save,
		promote:  promote,
		reparser: reparser,
	}
	for _, opt := range opts {
		opt(sr)
	}
	return sr, nil
}

// IsSyncing returns true while Run is in progress.
func (r *StartupResync) IsSyncing() bool {
	return r.syncing.Load()
}

// Run executes the resync for all repos. Blocks until complete or ctx is cancelled.
// For each repo:
//  1. Get HEAD SHA via git.HEAD
//  2. If last_promoted_sha == HEAD: skip (already up to date)
//  3. If last_promoted_sha is empty: full reparse path
//  4. If git.IsAncestor(sha, head): replay commits sha.HEAD via save + promote per commit
//  5. If not ancestor (divergent): full reparse path (log ErrPromotionDivergent, then reparse)
//
// Returns the first non-divergent error. Divergent errors are logged but not returned.
func (r *StartupResync) Run(ctx context.Context) error {
	r.syncing.Store(true)
	defer r.syncing.Store(false)

	repos, err := r.repos.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("startup resync: list repos: %w", err)
	}

	// Per-repo errors are LOGGED and SKIPPED — a single failure (e.g.
	// SQLITE_BUSY against the post-promotion queue, a missing working
	// tree, a parser crash) must not abort the rest of the resync.
	// Otherwise repos registered AFTER the failing one never get
	// indexed at all (: pflag+cobra promoted, logrus errored
	// on a sink-before-delete race, and sam-repo never started).
	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.resyncRepo(ctx, repo); err != nil {
			// a missing root is an expected, recurring state
			// (registered repo whose checkout has since moved or been
			// deleted). Log it as WARN so it doesn't cry wolf at every
			// boot; reserve ERROR for genuinely unexpected failures.
			level := slog.LevelError
			hint := ""
			if isMissingRoot(err) {
				level = slog.LevelWarn
				hint = "run `veska repo prune` or `veska repo remove " + string(repo.RepoID) + "` to clear"
			}
			slog.Log(ctx, level, "startup resync: repo failed; continuing with remaining repos",
				"repo_id", repo.RepoID,
				"root", repo.RootPath,
				"err", err,
				"hint", hint,
			)
			continue
		}
	}

	return nil
}

// resyncRepo handles resync for a single repo.
func (r *StartupResync) resyncRepo(ctx context.Context, repo RepoRecord) error {
	// begin by reconciling the working-tree branch against
	// repos.active_branch BEFORE any parse/replay. On a branch switch this
	// bumps the staging generation, drops the prior branch's staging, and
	// updates active_branch. The resolved branch flows back into the
	// loop-local RepoRecord so replayCommit keys save/promote on the NEW
	// branch (repo is a value copy — mutating it here is intended). A no-op
	// reconcile returns "" and leaves the existing branch key untouched.
	if r.br != nil {
		if b, _ := r.br.Reconcile(ctx, repo.RepoID, repo.RootPath); b != "" {
			repo.ActiveBranch = b
		}
	}

	head, err := r.git.HEAD(repo.RootPath)
	if err != nil {
		return fmt.Errorf("startup resync: HEAD for repo %q: %w", repo.RepoID, err)
	}

	// Already up to date — nothing to do.
	if repo.LastPromotedSHA == head {
		return nil
	}

	// Never promoted — full reparse.
	if repo.LastPromotedSHA == "" {
		slog.Info("startup resync: never-promoted repo; running full reparse",
			"repo_id", repo.RepoID,
		)
		return r.reparser(ctx, repo)
	}

	// Check if last_promoted_sha is reachable from HEAD.
	ancestor, err := r.git.IsAncestor(repo.RootPath, repo.LastPromotedSHA, head)
	if err != nil {
		return fmt.Errorf("startup resync: IsAncestor for repo %q: %w", repo.RepoID, err)
	}

	if !ancestor {
		// Divergent SHA — force-push or history rewrite.
		divErr := &ErrPromotionDivergent{RepoID: repo.RepoID, SHA: repo.LastPromotedSHA}
		slog.Warn("startup resync: divergent SHA; running full reparse",
			"repo_id", repo.RepoID,
			"sha", repo.LastPromotedSHA,
			"err", divErr,
			"degraded_reasons", []string{"startup_resync"},
		)
		return r.reparser(ctx, repo)
	}

	// Ancestor path — replay missed commits.
	commits, err := r.git.CommitsSince(repo.RootPath, repo.LastPromotedSHA, head)
	if err != nil {
		return fmt.Errorf("startup resync: CommitsSince for repo %q: %w", repo.RepoID, err)
	}

	slog.Info("startup resync: replaying missed commits",
		"repo_id", repo.RepoID,
		"commits", len(commits),
		"degraded_reasons", []string{"startup_resync"},
	)

	for _, sha := range commits {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.replayCommit(ctx, repo, sha); err != nil {
			return err
		}
	}

	return nil
}

// replayCommit processes a single commit by saving changed files then promoting.
func (r *StartupResync) replayCommit(ctx context.Context, repo RepoRecord, sha string) error {
	files, err := r.git.ChangedFiles(repo.RootPath, sha)
	if err != nil {
		return fmt.Errorf("startup resync: ChangedFiles for commit %q in repo %q: %w", sha, repo.RepoID, err)
	}

	for _, f := range files {
		src, err := r.git.ReadFileAtCommit(repo.RootPath, sha, f)
		if err != nil {
			// Non-fatal: file may have been deleted in this commit.
			slog.Warn("startup resync: ReadFileAtCommit failed; skipping file",
				"repo_id", repo.RepoID,
				"sha", sha,
				"file", f,
				"err", err,
			)
			continue
		}
		r.save(ctx, repo.RepoID, repo.ActiveBranch, f, src)
	}

	// Startup resync is always system-triggered.
	systemActor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
	return r.promote(ctx, repo.RepoID, repo.ActiveBranch, sha, systemActor)
}
