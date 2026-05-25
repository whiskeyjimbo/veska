// Package application contains use-case services that orchestrate the
// domain entities. Implementations of side-effecting ports (storage,
// parsers, embedding providers) are wired in from the infrastructure
// layer via constructors defined elsewhere in this package.
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
// dependency is nil. It is a package-wide sentinel so callers can use
// errors.Is to distinguish a wiring fault from a runtime failure.
var ErrMissingDependency = errors.New("application: missing required dependency")

// IgnoreMatcher is the application-layer port that decides whether a path
// should be excluded from the cold scan. The default ignores nothing; the
// daemon wires in an adapter over internal/infrastructure/fs.IgnoreList.
type IgnoreMatcher interface {
	ShouldIgnore(path string) bool
}

// IgnoreLoader builds an IgnoreMatcher for a given repository root. It is
// invoked once per cold scan so per-repo .veskaignore files can shape the
// resulting IgnoreMatcher.
type IgnoreLoader func(repoRoot string) (IgnoreMatcher, error)

// coldScanConfig holds the runtime knobs accumulated from ColdScanOptions.
type coldScanConfig struct {
	loader  IgnoreLoader
	tracker *ScanTracker
}

// ColdScanOption configures NewColdScanReparser at construction time.
type ColdScanOption func(*coldScanConfig)

// WithIgnoreLoader sets the IgnoreLoader used to build a per-repo
// IgnoreMatcher at scan time. Defaults to defaultIgnoreLoader (allow-all)
// when not supplied.
func WithIgnoreLoader(l IgnoreLoader) ColdScanOption {
	return func(c *coldScanConfig) { c.loader = l }
}

// WithScanTracker registers a ScanTracker the reparser will Start at scan
// entry and End at scan exit (solov2-pm5). The daemon's status handler
// reads the same tracker so eng_get_status can surface scans_in_flight.
// Nil-safe: when no tracker is wired the calls are no-ops.
func WithScanTracker(t *ScanTracker) ColdScanOption {
	return func(c *coldScanConfig) { c.tracker = t }
}

// allowAllMatcher is the zero-value IgnoreMatcher: it never ignores
// anything. Used as a safe fallback when no IgnoreLoader is wired.
type allowAllMatcher struct{}

func (allowAllMatcher) ShouldIgnore(string) bool { return false }

// defaultIgnoreLoader returns the zero IgnoreMatcher for any path. Wired
// when no WithIgnoreLoader was supplied so tests get a deterministic
// "ignore nothing" default.
func defaultIgnoreLoader(string) (IgnoreMatcher, error) { return allowAllMatcher{}, nil }

// coldScanSaveFn is the narrow surface NewColdScanReparser needs from
// Ingester.Save. Keeping the seam small makes test fakes trivial.
type coldScanSaveFn func(ctx context.Context, repoID, branch, path string, src []byte)

// coldScanPromoteFn is the narrow surface NewColdScanReparser needs from
// Promoter.Promote.
type coldScanPromoteFn func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error

// NewColdScanReparser returns the closure that StartupResync.reparser and
// repoRegistrar.AddRepo both invoke for a full-reparse path. It walks the
// repo's working tree, parses every non-ignored source file through the
// given Ingester, and finalises by promoting at HEAD with the system
// actor (solov2-0z1.1).
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
	// Wire the cold-scan-specific Save variant (solov2-pc3 #2): it
	// skips clearParseFailure for clean parses since there's nothing
	// to clear on a first-ever scan, removing one UPDATE per file on
	// the contended WriteHot pool.
	return newColdScanReparserFromFns(ingester.SaveColdScan, promoter.Promote, git, opts...)
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

		// Surface the in-flight state to eng_get_status (solov2-pm5).
		// The defer guarantees End fires on every exit path so a
		// failed scan doesn't pin the tracker entry forever. Nil-safe
		// when no tracker is wired (legacy / test callers).
		cfg.tracker.Start(repo.RepoID)
		defer cfg.tracker.End(repo.RepoID)
		cfg.tracker.SetPhase(repo.RepoID, "walking")

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
		// Also publish files_seen to the scan tracker after each save so
		// eng_get_status / veska repo list can show progress for long
		// scans (solov2-u9h9). files_total is left 0 until we have a
		// cheap upfront count — wiring that here would double-walk the
		// tree. Showing files_seen alone is still enough to tell hung
		// from progressing.
		filesSaved := 0
		countingSave := func(ctx context.Context, repoID, branch, path string, src []byte) {
			save(ctx, repoID, branch, path, src)
			filesSaved++
			// Throttle tracker updates: every 25 files is plenty for a
			// 'is it still moving?' signal and keeps tracker contention
			// negligible on the hot path.
			if filesSaved%25 == 0 {
				cfg.tracker.Progress(repo.RepoID, filesSaved, 0)
			}
		}

		if err := walkAndSave(ctx, repo, ignore, countingSave); err != nil {
			slog.Warn("cold scan: walk failed",
				"repo_id", repo.RepoID, "files_saved", filesSaved, "err", err)
			return err
		}
		// Publish the final walk count before flipping to promoting so a
		// user polling eng_get_status sees the true files_seen rather than
		// a multiple-of-25 snapshot, then learns we've entered the slow
		// promotion phase (solov2-u9h9).
		cfg.tracker.Progress(repo.RepoID, filesSaved, 0)
		cfg.tracker.SetPhase(repo.RepoID, "promoting")

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
// into save. Serial by design: parallelising the walker on a real repo
// (solov2-pc3 investigation) made wall time slightly worse because the
// daemon's per-file workload contends with the embedder / queue / wiki
// workers via the WriteHot pool and (likely) the page cache. A worker
// pool just spreads that contention across more goroutines without
// improving total throughput. The fix lives upstream of this walker.
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

		// solov2-8x7r: skip files the parsers can't handle AND that aren't
		// a known manifest some downstream check needs by basename
		// (vuln-scan reads go.mod). Without this filter the cold scan
		// staged a row for every README, .yml, .json, etc. — the result
		// was a wide PromotionBatch.Files list that bloated staging and
		// confused filename-gated checks.
		if !isInterestingForStaging(path) {
			return nil
		}

		save(ctx, repo.RepoID, repo.ActiveBranch, path, src)
		return nil
	})
}

// parseableExtensions enumerates the source-file extensions the wired
// parsers know how to read. Keep in sync with the treesitter parsers
// (treesitter.GoParser handles ".go"; TSParser handles ".ts" / ".tsx").
// Centralising it here avoids a circular dependency between the
// application layer and the parser adapter.
var parseableExtensions = map[string]struct{}{
	".go":  {},
	".ts":  {},
	".tsx": {},
}

// manifestBaseNames enumerates non-source files some structural check
// gates on by basename. Currently only go.mod (vuln-scan); other entries
// (package.json, Cargo.toml, …) can be added as scanners arrive.
var manifestBaseNames = map[string]struct{}{
	"go.mod": {},
}

// isInterestingForStaging reports whether a file should be fed to the
// ingester. Returns true for any source file whose extension a parser
// can read, or any manifest a downstream check gates on by basename.
func isInterestingForStaging(path string) bool {
	if _, ok := manifestBaseNames[filepath.Base(path)]; ok {
		return true
	}
	_, ok := parseableExtensions[filepath.Ext(path)]
	return ok
}

// isLikelyBinary reports whether src contains an embedded NUL in its first
// 8 KiB — a reliable cheap heuristic for binary blobs that should never be
// fed to the parser. Source files in supported languages never contain NUL.
func isLikelyBinary(src []byte) bool {
	n := min(len(src), 8192)
	return bytes.IndexByte(src[:n], 0) >= 0
}
