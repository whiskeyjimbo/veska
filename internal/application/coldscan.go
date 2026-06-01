package application

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

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
	// extensions is the set of source-file extensions the wired parser can
	// read, sourced from the parser via Ingester.SupportedExtensions rather
	// than duplicated here (solov2-xde2.7).
	extensions map[string]struct{}
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
// entry and End at scan exit . The daemon's status handler
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

// saveFunc is the narrow seam over Ingester.Save shared by the cold-scan and
// startup-resync paths. Keeping the seam small makes test fakes trivial.
type saveFunc func(ctx context.Context, repoID, branch, path string, src []byte)

// promoteFunc is the narrow seam over Promoter.Promote, shared by the
// cold-scan and startup-resync paths.
type promoteFunc func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error

// headQuerier is the narrow slice of GitQuerier the cold-scan reparser
// actually uses: just the current HEAD SHA. Declaring it locally (ISP) keeps
// the cold-scan seam honest about its single dependency. Any GitQuerier
// satisfies it, so production and test callers are unchanged.
type headQuerier interface {
	HEAD(rootPath string) (string, error)
}

// NewColdScanReparser returns the closure that StartupResync.reparser and
// repoRegistrar.AddRepo both invoke for a full-reparse path. It walks the
// repo's working tree, parses every non-ignored source file through the
// given Ingester, and finalises by promoting at HEAD with the system
// actor (solov2-0z1.1).
func NewColdScanReparser(ingester *Ingester, promoter *Promoter, git headQuerier, opts ...ColdScanOption) (func(ctx context.Context, repo RepoRecord) error, error) {
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
	// the contended Write pool.
	return newColdScanReparserFromFns(ingester.SaveColdScan, promoter.Promote, git, ingester.SupportedExtensions(), opts...)
}

// newColdScanReparserFromFns is the internal seam: it composes a reparser
// closure from raw save/promote function values, decoupled from the
// concrete Ingester/Promoter types so unit tests can capture invocations
// without spinning up the real pipeline. The public constructor wires
// Ingester.Save and Promoter.Promote here.
func newColdScanReparserFromFns(save saveFunc, promote promoteFunc, git headQuerier, exts []string, opts ...ColdScanOption) (func(ctx context.Context, repo RepoRecord) error, error) {
	if save == nil || promote == nil || git == nil {
		return nil, fmt.Errorf("application.newColdScanReparserFromFns: nil seam: %w", ErrMissingDependency)
	}

	cfg := coldScanConfig{loader: defaultIgnoreLoader, extensions: extensionSet(exts)}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.loader == nil {
		cfg.loader = defaultIgnoreLoader
	}

	seams := coldScanSeams{save: save, promote: promote, git: git}
	return func(ctx context.Context, repo RepoRecord) error {
		return runColdScan(ctx, repo, cfg, seams)
	}, nil
}

// coldScanActor is the system actor under which cold-scan promotions are
// recorded (solov2-0z1.1).
var coldScanActor = domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}

// coldScanSeams bundles the (required) function seams a cold scan drives:
// staging a file, promoting the batch, and reading HEAD. Grouping them keeps
// runColdScan within the argument budget and names the collaboration.
type coldScanSeams struct {
	save    saveFunc
	promote promoteFunc
	git     headQuerier
}

// runColdScan brackets one cold scan: start/complete INFO logs (so an operator
// tailing ~/.veska/logs/daemon.log can tell work is in flight), tracker
// start/defer-end, HEAD + ignore-list resolution, the walk, and the final
// promote at HEAD.
func runColdScan(ctx context.Context, repo RepoRecord, cfg coldScanConfig, seams coldScanSeams) error {
	start := time.Now()
	slog.Info("cold scan: starting",
		"repo_id", repo.RepoID, "root", repo.RootPath, "branch", repo.ActiveBranch)

	// The defer guarantees End fires on every exit path so a failed scan
	// doesn't pin the tracker entry forever. Nil-safe when no tracker is wired.
	cfg.tracker.Start(repo.RepoID)
	defer cfg.tracker.End(repo.RepoID)
	cfg.tracker.SetPhase(repo.RepoID, "walking")

	head, err := seams.git.HEAD(repo.RootPath)
	if err != nil {
		slog.Warn("cold scan: HEAD lookup failed", "repo_id", repo.RepoID, "err", err)
		return fmt.Errorf("cold scan: HEAD for repo %q: %w", repo.RepoID, err)
	}

	ignore, err := cfg.loader(repo.RootPath)
	if err != nil {
		slog.Warn("cold scan: load ignore list failed", "repo_id", repo.RepoID, "err", err)
		return fmt.Errorf("cold scan: load ignore list for repo %q: %w", repo.RepoID, err)
	}

	filesSaved, err := walkPhase(ctx, repo, cfg, ignore, seams.save)
	if err != nil {
		slog.Warn("cold scan: walk failed",
			"repo_id", repo.RepoID, "files_saved", filesSaved, "err", err)
		return err
	}
	cfg.tracker.SetPhase(repo.RepoID, "promoting")

	if err := seams.promote(ctx, repo.RepoID, repo.ActiveBranch, head, coldScanActor); err != nil {
		slog.Warn("cold scan: promote failed",
			"repo_id", repo.RepoID, "files_saved", filesSaved, "err", err)
		return err
	}

	slog.Info("cold scan: complete",
		"repo_id", repo.RepoID, "files_saved", filesSaved, "git_sha", head,
		"elapsed_ms", time.Since(start).Milliseconds())
	return nil
}

// walkPhase walks the repo and feeds surviving files into save, returning the
// number of files staged. It wraps save with a per-file counter that publishes
// files_seen to the scan tracker so eng_get_status / veska repo list show
// progress on long scans (files_total stays 0 — a cheap upfront count would
// double-walk the tree; files_seen alone tells hung from progressing). The
// final Progress call before returning ensures the count reflects the true
// total rather than a throttled snapshot.
func walkPhase(ctx context.Context, repo RepoRecord, cfg coldScanConfig, ignore IgnoreMatcher, save saveFunc) (int, error) {
	filesSaved := 0
	countingSave := func(ctx context.Context, repoID, branch, path string, src []byte) {
		save(ctx, repoID, branch, path, src)
		filesSaved++
		cfg.tracker.Progress(repo.RepoID, filesSaved, 0)
	}
	err := walkAndSave(ctx, repo, ignore, cfg.extensions, countingSave)
	cfg.tracker.Progress(repo.RepoID, filesSaved, 0)
	return filesSaved, err
}

// walkAndSave performs the filesystem walk and feeds each surviving file
// into save. Serial by design: parallelising the walker on a real repo
// (solov2-pc3 investigation) made wall time slightly worse because the
// daemon's per-file workload contends with the embedder / queue / wiki
// workers via the Write pool and (likely) the page cache. A worker
// pool just spreads that contention across more goroutines without
// improving total throughput. The fix lives upstream of this walker.
func walkAndSave(ctx context.Context, repo RepoRecord, ignore IgnoreMatcher, exts map[string]struct{}, save saveFunc) error {
	root := repo.RootPath
	stager := fileStager{repo: repo, ignore: ignore, exts: exts, save: save}
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
			return walkDirEntry(rel, d, ignore)
		}
		stager.stage(ctx, path, rel, d)
		return nil
	})
}

// walkDirEntry decides what the walk does with a directory: descend (nil) or
// prune (SkipDir). .git is always pruned; otherwise the trailing-slash form
// lets directory ignore patterns match.
func walkDirEntry(rel string, d fs.DirEntry, ignore IgnoreMatcher) error {
	if rel == "." {
		return nil
	}
	if d.Name() == ".git" {
		return filepath.SkipDir
	}
	if ignore.ShouldIgnore(rel + "/") {
		return filepath.SkipDir
	}
	return nil
}

// fileStager holds the walk-invariant collaborators (repo, ignore matcher,
// parser extension set, save seam) so the per-file stage decision is a method
// with a small argument list rather than an 8-parameter free function.
type fileStager struct {
	repo   RepoRecord
	ignore IgnoreMatcher
	exts   map[string]struct{}
	save   saveFunc
}

// stage feeds one walked file into save when it survives every filter:
// regular-file, not .veskaignore, not ignored, readable, non-binary, and an
// interesting extension/manifest (solov2-8x7r — without the last filter the
// scan staged a row for every README/.yml/.json, bloating the PromotionBatch
// and confusing filename-gated checks). A read failure is skipped silently,
// matching the watch loop's tolerance so one bad file never aborts the scan.
func (s fileStager) stage(ctx context.Context, path, rel string, d fs.DirEntry) {
	if !d.Type().IsRegular() {
		return
	}
	if d.Name() == ".veskaignore" {
		return
	}
	if s.ignore.ShouldIgnore(rel) {
		return
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return
	}
	// Fast NUL-byte sniff: binary blobs (lockfiles, minified assets) would
	// otherwise churn the parser for no value.
	if isLikelyBinary(src) {
		return
	}
	if !isInterestingForStaging(path, s.exts) {
		return
	}
	s.save(ctx, s.repo.RepoID, s.repo.ActiveBranch, path, src)
}

// extensionSet folds the wired parser's SupportedExtensions list into a
// lookup set, lower-casing each so the walk filter matches regardless of
// on-disk case (solov2-xde2.7). The list is sourced from the parser via
// Ingester.SupportedExtensions rather than duplicated here, so it stays in
// sync with whatever parsers are actually wired into the cold scan.
func extensionSet(exts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(exts))
	for _, ext := range exts {
		set[strings.ToLower(ext)] = struct{}{}
	}
	return set
}

// manifestBaseNames enumerates non-source files some structural check
// gates on by basename. Currently only go.mod (vuln-scan); other entries
// (package.json, Cargo.toml, …) can be added as scanners arrive.
var manifestBaseNames = map[string]struct{}{
	"go.mod": {},
}

// isInterestingForStaging reports whether a file should be fed to the
// ingester. Returns true for any source file whose extension the wired
// parser can read (exts), or any manifest a downstream check gates on by
// basename.
func isInterestingForStaging(path string, exts map[string]struct{}) bool {
	if _, ok := manifestBaseNames[filepath.Base(path)]; ok {
		return true
	}
	_, ok := exts[strings.ToLower(filepath.Ext(path))]
	return ok
}

// isLikelyBinary reports whether src contains an embedded NUL in its first
// 8 KiB — a reliable cheap heuristic for binary blobs that should never be
// fed to the parser. Source files in supported languages never contain NUL.
func isLikelyBinary(src []byte) bool {
	n := min(len(src), 8192)
	return bytes.IndexByte(src[:n], 0) >= 0
}
