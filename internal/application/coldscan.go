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

// IgnoreMatcher decides whether a path should be excluded from the cold scan.
type IgnoreMatcher interface {
	ShouldIgnore(path string) bool
}

// IgnoreLoader builds an IgnoreMatcher for a repository root, reading per-repo
// ignore configs.
type IgnoreLoader func(repoRoot string) (IgnoreMatcher, error)

type coldScanConfig struct {
	loader     IgnoreLoader
	tracker    *ScanTracker
	extensions map[string]struct{}
}

type ColdScanOption func(*coldScanConfig)

// WithIgnoreLoader configures the IgnoreLoader for the reparser.
func WithIgnoreLoader(l IgnoreLoader) ColdScanOption {
	return func(c *coldScanConfig) { c.loader = l }
}

// WithScanTracker registers a ScanTracker to report ongoing scans to the status handler.
func WithScanTracker(t *ScanTracker) ColdScanOption {
	return func(c *coldScanConfig) { c.tracker = t }
}

// allowAllMatcher is a fallback IgnoreMatcher that never ignores files.
type allowAllMatcher struct{}

func (allowAllMatcher) ShouldIgnore(string) bool { return false }

func defaultIgnoreLoader(string) (IgnoreMatcher, error) { return allowAllMatcher{}, nil }

// saveFunc abstracts Ingester.Save for unit testing.
type saveFunc func(ctx context.Context, repoID, branch, path string, src []byte)

// promoteFunc abstracts Promoter.Promote for unit testing.
type promoteFunc func(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error

// headQuerier abstracts GitQuerier.HEAD to retrieve the current repository revision.
type headQuerier interface {
	HEAD(rootPath string) (string, error)
}

// NewColdScanReparser creates a function to walk the workspace, parse source files,
// and promote them at HEAD.
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
	// Use SaveColdScan to skip clean parse updates on initial run and reduce write
	// lock contention.
	return newColdScanReparserFromFns(ingester.SaveColdScan, promoter.Promote, git, ingester.SupportedExtensions(), opts...)
}

// newColdScanReparserFromFns is the internal seam decoupling concrete
// Ingester/Promoter implementations for unit testing.
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

var coldScanActor = domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}

// coldScanSeams bundles the required staging and repository query functions.
type coldScanSeams struct {
	save    saveFunc
	promote promoteFunc
	git     headQuerier
}

// runColdScan executes a single cold scan pass, logging progress and updating scan
// status tracker.
func runColdScan(ctx context.Context, repo RepoRecord, cfg coldScanConfig, seams coldScanSeams) error {
	start := time.Now()
	slog.Info("cold scan: starting",
		"repo_id", repo.RepoID, "root", repo.RootPath, "branch", repo.ActiveBranch)

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

// walkPhase runs walkAndSave, logging files seen incrementally to report progress
// on large repositories.
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

// walkAndSave walks the filesystem sequentially; parallel walkers increase write
// contention and degrade throughput.
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

// walkDirEntry skips the .git folder or directories matching ignore patterns.
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

// fileStager collects configuration states to keep stage method signatures narrow.
type fileStager struct {
	repo   RepoRecord
	ignore IgnoreMatcher
	exts   map[string]struct{}
	save   saveFunc
}

// stage filters and stages a file if it matches valid extensions or lockfiles,
// ignoring binary blobs.
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
	// The ingester key relies on repo-relative slash paths, while read bytes use
	// absolute paths.
	s.save(ctx, s.repo.RepoID, s.repo.ActiveBranch, rel, src)
}

// extensionSet registers parser extensions in lowercase to support
// case-insensitive checks.
func extensionSet(exts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(exts))
	for _, ext := range exts {
		set[strings.ToLower(ext)] = struct{}{}
	}
	return set
}

// manifestBaseNames maps non-source files (such as go.mod) that trigger
// security check scanners.
var manifestBaseNames = map[string]struct{}{
	"go.mod": {},
}

// isInterestingForStaging filters files by matching extensions or manifest basenames.
func isInterestingForStaging(path string, exts map[string]struct{}) bool {
	if _, ok := manifestBaseNames[filepath.Base(path)]; ok {
		return true
	}
	_, ok := exts[strings.ToLower(filepath.Ext(path))]
	return ok
}

// isLikelyBinary checks the first 8KiB for a NUL byte to identify binary files.
func isLikelyBinary(src []byte) bool {
	n := min(len(src), 8192)
	return bytes.IndexByte(src[:n], 0) >= 0
}
