package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ErrMissingDependency is returned by NewColdScanReparser when a required
// collaborator (ingester, promoter, git querier) is nil. It is the
// application-package sentinel used by the cold-scan constructor; other
// services inside the package re-use it as the layer-wide convention.
var ErrMissingDependency = errors.New("application: missing required dependency")

// IgnoreMatcher decides whether a repo-root-relative path should be skipped
// by the cold-scan walker. Directory matches are signalled by the caller
// passing a trailing-slash form ("vendor/"); concrete implementations live
// in infrastructure (see internal/infrastructure/fs.IgnoreList) — the
// application layer references only this contract to keep the dependency
// graph pointing inward.
type IgnoreMatcher interface {
	ShouldIgnore(path string) bool
}

// IgnoreLoader resolves the IgnoreMatcher for a given repo working tree.
// The default loader used by NewColdScanReparser is a no-op (no ignores);
// production wiring in cmd/veska-daemon injects an adapter over
// infrastructure/fs.Load so the application layer never imports it directly.
type IgnoreLoader func(repoRoot string) (IgnoreMatcher, error)

// coldScanSaveFn matches Ingester.Save. Declared as a type alias so the
// cold-scan internal seam can be driven directly in unit tests without
// constructing a full Ingester.
type coldScanSaveFn = func(ctx context.Context, repoID, branch, path string, src []byte)

// coldScanPromoteFn matches Promoter.Promote. Declared for the same reason
// as coldScanSaveFn.
type coldScanPromoteFn = func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error

// ColdScanOption configures NewColdScanReparser. Currently the only knob is
// WithIgnoreLoader, but the functional-options shape leaves room for adding
// e.g. a custom file reader without rewriting the constructor.
type ColdScanOption func(*coldScanConfig)

type coldScanConfig struct {
	loader IgnoreLoader
}

// WithIgnoreLoader installs a non-default IgnoreLoader. Production wiring
// passes an adapter over infrastructure/fs.Load; tests pass an in-memory
// matcher built from explicit patterns.
func WithIgnoreLoader(l IgnoreLoader) ColdScanOption {
	return func(c *coldScanConfig) { c.loader = l }
}

// allowAllMatcher matches nothing — used as the safe default loader so
// callers that forget to inject one still get a working reparser (it will
// just index every file). Production wiring always overrides this.
type allowAllMatcher struct{}

func (allowAllMatcher) ShouldIgnore(string) bool { return false }

func defaultIgnoreLoader(string) (IgnoreMatcher, error) { return allowAllMatcher{}, nil }

// NewColdScanReparser returns a reparser closure that satisfies
// StartupResync's `reparser func(ctx, RepoRecord) error` hook contract. The
// closure walks a repo's working tree, streams every non-ignored regular
// file through ingester.Save, then promotes the resulting staging slot at
// HEAD with a system actor.
//
// The reparser is sequential — no goroutine fan-out — and honours ctx
// cancellation between file visits. Promotion is skipped when ctx is
// cancelled or when HEAD lookup fails.
//
// Returns an error wrapping ErrMissingDependency if any required dependency
// is nil.
func NewColdScanReparser(ingester *Ingester, promoter *Promoter, git GitQuerier, opts ...ColdScanOption) (func(ctx context.Context, repo RepoRecord) error, error) {
	if ingester == nil {
		return nil, fmt.Errorf("application.NewColdScanReparser: ingester is nil: %w", ErrMissingDependency)
	}
	if promoter == nil {
		return nil, fmt.Errorf("application.NewColdScanReparser: promoter is nil: %w", ErrMissingDependency)
	}
	if git == nil {
		return nil, fmt.Errorf("application.NewColdScanReparser: git is nil: %w", ErrMissingDependency)
	}
	return newColdScanReparserFromFns(ingester.Save, promoter.Promote, git, opts...)
}

// newColdScanReparserFromFns is the internal seam: it composes a reparser
// closure from raw save/promote function values, decoupled from the
// concrete Ingester/Promoter types so unit tests can capture invocations
// without spinning up the real pipeline. The public constructor wires
// Ingester.Save and Promoter.Promote here.
func newColdScanReparserFromFns(save coldScanSaveFn, promote coldScanPromoteFn, git GitQuerier, opts ...ColdScanOption) (func(ctx context.Context, repo RepoRecord) error, error) {
	if save == nil || promote == nil || git == nil {
		return nil, fmt.Errorf("application.newColdScanReparserFromFns: nil seam: %w", ErrMissingDependency)
	}

	cfg := coldScanConfig{loader: defaultIgnoreLoader}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.loader == nil {
		cfg.loader = defaultIgnoreLoader
	}

	systemActor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}

	return func(ctx context.Context, repo RepoRecord) error {
		// Bracket every scan with start + complete INFO logs so an
		// operator tailing ~/.veska/logs/daemon.log can tell that work
		// is in flight and observe completion (solov2-6ip). Previously
		// the cold-scan path was silent end-to-end; a newbie running
		// 'veska repo add <big repo>' saw nothing and assumed it had
		// hung.
		start := time.Now()
		slog.Info("cold scan: starting",
			"repo_id", repo.RepoID,
			"root", repo.RootPath,
			"branch", repo.ActiveBranch,
		)

		head, err := git.HEAD(repo.RootPath)
		if err != nil {
			slog.Warn("cold scan: HEAD lookup failed",
				"repo_id", repo.RepoID, "err", err)
			return fmt.Errorf("cold scan: HEAD for repo %q: %w", repo.RepoID, err)
		}

		ignore, err := cfg.loader(repo.RootPath)
		if err != nil {
			slog.Warn("cold scan: load ignore list failed",
				"repo_id", repo.RepoID, "err", err)
			return fmt.Errorf("cold scan: load ignore list for repo %q: %w", repo.RepoID, err)
		}

		// Wrap save so we can report files_saved at completion. The
		// wrapper is cheap (a counter increment on the same goroutine
		// the walk runs on, no locking) and keeps walkAndSave's
		// signature unchanged.
		filesSaved := 0
		countingSave := func(ctx context.Context, repoID, branch, path string, src []byte) {
			save(ctx, repoID, branch, path, src)
			filesSaved++
		}

		if err := walkAndSave(ctx, repo, ignore, countingSave); err != nil {
			slog.Warn("cold scan: walk failed",
				"repo_id", repo.RepoID, "files_saved", filesSaved, "err", err)
			return err
		}

		if err := promote(ctx, repo.RepoID, repo.ActiveBranch, head, systemActor); err != nil {
			slog.Warn("cold scan: promote failed",
				"repo_id", repo.RepoID, "files_saved", filesSaved, "err", err)
			return err
		}

		slog.Info("cold scan: complete",
			"repo_id", repo.RepoID,
			"files_saved", filesSaved,
			"git_sha", head,
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
		return nil
	}, nil
}

// walkAndSave performs the filesystem walk and feeds each surviving file
// into save. It is factored out of the closure for testability and to keep
// the closure body readable.
func walkAndSave(ctx context.Context, repo RepoRecord, ignore IgnoreMatcher, save coldScanSaveFn) error {
	root := repo.RootPath
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("cold scan: rel path for %q: %w", path, err)
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel == "." {
				return nil
			}
			// .git is always pruned regardless of ignore patterns.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			// Use trailing-slash form so directory patterns match.
			if ignore.ShouldIgnore(rel + "/") {
				return filepath.SkipDir
			}
			return nil
		}

		// Regular files only — skip symlinks, devices, sockets, etc.
		if !d.Type().IsRegular() {
			return nil
		}
		// The ignore-file itself is metadata, never source.
		if d.Name() == ".veskaignore" {
			return nil
		}
		if ignore.ShouldIgnore(rel) {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			// Match the watch-loop's tolerance: a single unreadable file
			// must not abort the whole scan.
			return nil
		}

		// Fast NUL-byte sniff: binary blobs (lockfiles, minified assets)
		// would otherwise churn the parser for no value.
		if isLikelyBinary(src) {
			return nil
		}

		save(ctx, repo.RepoID, repo.ActiveBranch, path, src)
		return nil
	})
}

// isLikelyBinary reports whether src contains an embedded NUL in its first
// 8 KiB — a reliable cheap heuristic for binary blobs that should never be
// fed to the parser. Source files in supported languages never contain NUL.
func isLikelyBinary(src []byte) bool {
	n := min(len(src), 8192)
	return bytes.IndexByte(src[:n], 0) >= 0
}
