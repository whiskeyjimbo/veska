// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// isMissingRoot reports whether a repository root directory does not exist on
// disk by checking the error message.
func isMissingRoot(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "No such file or directory") || strings.Contains(s, "no such file or directory")
}

type RepoRecord struct {
	RepoID          string
	RootPath        string
	ActiveBranch    string
	LastPromotedSHA string
	Kind            string
	Aliases         []string
}

type RepoLister interface {
	ListRepos(ctx context.Context) ([]RepoRecord, error)
}

type GitQuerier interface {
	HEAD(rootPath string) (string, error)
	IsAncestor(rootPath, sha, head string) (bool, error)
	CommitsSince(rootPath, sha, head string) ([]string, error)
	ChangedFiles(rootPath, sha string) ([]string, error)
	ReadFileAtCommit(rootPath, sha, filePath string) ([]byte, error)
}

// ErrPromotionDivergent occurs when the last promoted commit is unreachable
// from HEAD.
type ErrPromotionDivergent struct {
	RepoID string
	SHA    string
}

func (e *ErrPromotionDivergent) Error() string {
	return fmt.Sprintf("veska: promotion divergent for repo %q: SHA %q is not reachable from HEAD", e.RepoID, e.SHA)
}

// StartupResync runs the startup index alignment for registered repos before the
// daemon accepts client connections.
type StartupResync struct {
	repos    RepoLister
	git      GitQuerier
	save     saveFunc
	promote  promoteFunc
	reparser func(ctx context.Context, repo RepoRecord) error

	br *BranchReconciler

	syncing atomic.Bool
}

// StartupResyncOption configures optional StartupResync behavior.
type StartupResyncOption func(*StartupResync)

// WithBranchReconciler registers a branch reconciler to check branch switches
// before replaying.
func WithBranchReconciler(br *BranchReconciler) StartupResyncOption {
	return func(s *StartupResync) {
		if br != nil {
			s.br = br
		}
	}
}

// NewStartupResync initializes StartupResync using functional interfaces for
// testing simplicity.
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

func (r *StartupResync) IsSyncing() bool {
	return r.syncing.Load()
}

// Run aligns all repositories with their disk HEADs. It is run-once and
// blocks until completed.
func (r *StartupResync) Run(ctx context.Context) error {
	r.syncing.Store(true)
	defer r.syncing.Store(false)

	repos, err := r.repos.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("startup resync: list repos: %w", err)
	}

	// Per-repo failures are caught and logged to prevent a single corrupted or
	// missing repository root from blocking others.
	for _, repo := range repos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.resyncRepo(ctx, repo); err != nil {
			// A missing repository directory is logged as a warning, while unexpected
			// errors log as errors.
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

func (r *StartupResync) resyncRepo(ctx context.Context, repo RepoRecord) error {
	// Reconciling branch switches before parsing updates staging active branch mappings.
	if r.br != nil {
		if b, _ := r.br.Reconcile(ctx, repo.RepoID, repo.RootPath); b != "" {
			repo.ActiveBranch = b
		}
	}

	head, err := r.git.HEAD(repo.RootPath)
	if err != nil {
		return fmt.Errorf("startup resync: HEAD for repo %q: %w", repo.RepoID, err)
	}

	if repo.LastPromotedSHA == head {
		return nil
	}

	if repo.LastPromotedSHA == "" {
		slog.Info("startup resync: never-promoted repo; running full reparse",
			"repo_id", repo.RepoID,
		)
		return r.reparser(ctx, repo)
	}

	ancestor, err := r.git.IsAncestor(repo.RootPath, repo.LastPromotedSHA, head)
	if err != nil {
		return fmt.Errorf("startup resync: IsAncestor for repo %q: %w", repo.RepoID, err)
	}

	if !ancestor {
		divErr := &ErrPromotionDivergent{RepoID: repo.RepoID, SHA: repo.LastPromotedSHA}
		slog.Warn("startup resync: divergent SHA; running full reparse",
			"repo_id", repo.RepoID,
			"sha", repo.LastPromotedSHA,
			"err", divErr,
			"degraded_reasons", []string{"startup_resync"},
		)
		return r.reparser(ctx, repo)
	}

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

func (r *StartupResync) replayCommit(ctx context.Context, repo RepoRecord, sha string) error {
	files, err := r.git.ChangedFiles(repo.RootPath, sha)
	if err != nil {
		return fmt.Errorf("startup resync: ChangedFiles for commit %q in repo %q: %w", sha, repo.RepoID, err)
	}

	for _, f := range files {
		src, err := r.git.ReadFileAtCommit(repo.RootPath, sha, f)
		if err != nil {
			// ReadFileAtCommit returns an error if the file was deleted in this commit,
			// which is expected and skipped.
			continue
		}
		r.save(ctx, repo.RepoID, repo.ActiveBranch, f, src)
	}

	systemActor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
	return r.promote(ctx, repo.RepoID, repo.ActiveBranch, sha, systemActor)
}
