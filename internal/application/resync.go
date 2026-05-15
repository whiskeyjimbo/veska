package application

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RepoRecord is the minimal view of a repos row needed for resync.
type RepoRecord struct {
	RepoID          string
	RootPath        string // absolute path to the git working tree
	ActiveBranch    string
	LastPromotedSHA string // empty = never promoted
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
	// CommitsSince returns the list of commit SHAs from sha..HEAD in oldest-first order,
	// i.e. the output of `git log <sha>..HEAD --reverse --format=%H`.
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
// During resync, IsSyncing() returns true.
type StartupResync struct {
	repos    RepoLister
	git      GitQuerier
	save     func(ctx context.Context, repoID, branch, path string, src []byte)
	promote  func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error
	reparser func(ctx context.Context, repo RepoRecord) error

	syncing atomic.Bool
}

// NewStartupResync constructs a StartupResync wired to the provided dependencies.
// saveFn and promoteFn wrap Ingester.Save and Promoter.Promote respectively.
// reparser is called for full-reparse paths (never-promoted or divergent SHA).
func NewStartupResync(
	repos RepoLister,
	git GitQuerier,
	ingester *Ingester,
	promoter *Promoter,
	reparser func(ctx context.Context, repo RepoRecord) error,
) *StartupResync {
	return &StartupResync{
		repos:    repos,
		git:      git,
		save:     ingester.Save,
		promote:  promoter.Promote,
		reparser: reparser,
	}
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
//  4. If git.IsAncestor(sha, head): replay commits sha..HEAD via save + promote per commit
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

	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.resyncRepo(ctx, repo); err != nil {
			return err
		}
	}

	return nil
}

// resyncRepo handles resync for a single repo.
func (r *StartupResync) resyncRepo(ctx context.Context, repo RepoRecord) error {
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
