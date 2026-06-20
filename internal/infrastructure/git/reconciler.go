// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	infrafs "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
)

// prefixLen specifies the number of leading bytes compared during a wake sweep in addition
// to mtime and size. Because some editors produce same-length modifications and some filesystems
// have coarse mtime granularity, comparing the prefix helps catch edits during system suspend without
// incurring the overhead of hashing full file contents.
const prefixLen = 64

// MtimeEntry records the last-seen modification time, size, and prefix of a file for change detection.
type MtimeEntry struct {
	ModTime time.Time
	Size    int64
	Prefix  [prefixLen]byte
}

// changedFrom reports whether the current entry differs from a previous entry in modification time, size, or prefix.
func (e MtimeEntry) changedFrom(prev MtimeEntry) bool {
	return e.ModTime != prev.ModTime || e.Size != prev.Size || e.Prefix != prev.Prefix
}

// SweepStartHook is invoked serially for each registered repository at the start of a wake sweep
// before any parallel walk begins. It is passed the repository identifier and root directory.
type SweepStartHook func(ctx context.Context, repoID, dir string)

// PostSweepHook is invoked once at the end of a wake sweep after all parallel walks have completed.
// It is skipped if the sweep is canceled before completion.
type PostSweepHook func(ctx context.Context)

// ReconcileHandler is called with the repository identifier and absolute path of each modified file.
type ReconcileHandler func(ctx context.Context, repoID, path string)

// watchedDir associates a directory tree with its owning repository.
type watchedDir struct {
	repoID string
	dir    string
}

// BaselineStore represents the interface used to query and update the file baselines.
// Implementations must be safe for concurrent use since they are shared between wake sweeps and live writes.
type BaselineStore interface {
	Get(path string) (MtimeEntry, bool)
	Put(path string, e MtimeEntry)
}

// BaselineResolver returns the active BaselineStore for a given repository. It returns false if
// the repository is not currently being tracked.
type BaselineResolver func(repoID string) (BaselineStore, bool)

// memBaseline implements an in-memory fallback baseline store used when no baseline resolver is configured.
// It is populated lazily on the first sighting of each file.
type memBaseline struct {
	mu sync.Mutex
	m  map[string]MtimeEntry
}

func newMemBaseline() *memBaseline { return &memBaseline{m: make(map[string]MtimeEntry)} }

func (b *memBaseline) Get(path string) (MtimeEntry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[path]
	return e, ok
}

func (b *memBaseline) Put(path string, e MtimeEntry) {
	b.mu.Lock()
	b.m[path] = e
	b.mu.Unlock()
}

// WakeReconciler monitors system suspend/resume events by tracking elapsed wall-clock time
// across periodic ticks. If a gap exceeds wakeThreshold, it performs a sweep across registered
// directories to detect modified files and triggers the handler.
type WakeReconciler struct {
	wakeTick      time.Duration
	wakeThreshold time.Duration
	handler       ReconcileHandler
	sweepStart    SweepStartHook
	postSweep     PostSweepHook
	ignore        *infrafs.IgnoreList
	nowFn         func() time.Time
	// concurrency limits the number of parallel sweeps executed across repositories.
	concurrency int

	// baselineFor resolves the active BaselineStore for a repository dynamically on each sweep.
	baselineFor BaselineResolver
	// standalone is the fallback baseline used when no resolver is configured or the repository is untracked.
	standalone *memBaseline

	mu       sync.Mutex
	dirs     []watchedDir
	lastTick time.Time // guarded by mu
	// reconciling maps repository identifiers to their in-progress sweep count.
	reconciling map[string]int

	// wakeCh is used to request an immediate, manual reconciliation sweep.
	wakeCh chan struct{}
}

// Option configures a WakeReconciler at construction.
type Option func(*WakeReconciler)

// WithClock overrides the default time source. This option is primarily intended for unit testing.
func WithClock(nowFn func() time.Time) Option {
	return func(r *WakeReconciler) {
		if nowFn != nil {
			r.nowFn = nowFn
		}
	}
}

// WithIgnoreList registers an ignore list to exclude specific files from sweep detection.
func WithIgnoreList(ignore *infrafs.IgnoreList) Option {
	return func(r *WakeReconciler) { r.ignore = ignore }
}

// WithSweepStartHook registers a hook called before the file-walk phase begins.
func WithSweepStartHook(fn SweepStartHook) Option {
	return func(r *WakeReconciler) { r.sweepStart = fn }
}

// WithPostSweepHook registers a hook called after all parallel file walks have completed. It is skipped on context cancellation.
func WithPostSweepHook(fn PostSweepHook) Option {
	return func(r *WakeReconciler) { r.postSweep = fn }
}

// WithBaseline configures the resolver providing the active change baseline.
func WithBaseline(fn BaselineResolver) Option {
	return func(r *WakeReconciler) { r.baselineFor = fn }
}

// WithWakeConcurrency limits the number of parallel sweeps executed across repositories.
func WithWakeConcurrency(n int) Option {
	return func(r *WakeReconciler) {
		if n <= 0 {
			n = runtime.NumCPU() / 2
		}
		if n < 1 {
			n = 1
		}
		r.concurrency = n
	}
}

// NewWakeReconciler constructs a new WakeReconciler instance.
func NewWakeReconciler(
	wakeTick time.Duration,
	wakeThreshold time.Duration,
	handler ReconcileHandler,
	opts ...Option,
) *WakeReconciler {
	r := &WakeReconciler{
		wakeTick:      wakeTick,
		wakeThreshold: wakeThreshold,
		handler:       handler,
		nowFn:         time.Now,
		standalone:    newMemBaseline(),
		reconciling:   make(map[string]int),
		wakeCh:        make(chan struct{}, 1),
	}
	WithWakeConcurrency(0)(r) // resolve the default; an explicit opt overrides below.
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// AddDir registers a repository's root directory for wake reconciliation.
func (r *WakeReconciler) AddDir(repoID, dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirs = append(r.dirs, watchedDir{repoID: repoID, dir: dir})
}

// IsRepoReconciling reports whether a sweep is currently active for the repository.
func (r *WakeReconciler) IsRepoReconciling(repoID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconciling[repoID] > 0
}

// ReconcilingRepos returns a sorted list of repository identifiers that are currently undergoing a sweep.
func (r *WakeReconciler) ReconcilingRepos() []string {
	r.mu.Lock()
	out := make([]string, 0, len(r.reconciling))
	for id := range r.reconciling {
		out = append(out, id)
	}
	r.mu.Unlock()
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// markReconciling tracks that a repository sweep has commenced.
func (r *WakeReconciler) markReconciling(repoID string) {
	r.mu.Lock()
	r.reconciling[repoID]++
	r.mu.Unlock()
}

// clearReconciling tracks that a repository sweep has completed.
func (r *WakeReconciler) clearReconciling(repoID string) {
	r.mu.Lock()
	if r.reconciling[repoID] <= 1 {
		delete(r.reconciling, repoID)
	} else {
		r.reconciling[repoID]--
	}
	r.mu.Unlock()
}

// wallTick returns the current time with its monotonic reading stripped. Monotonic time does
// not advance during system suspend, so stripping it ensures that duration subtraction reflects
// wall-clock elapsed time across sleep states.
func (r *WakeReconciler) wallTick() time.Time {
	return r.nowFn().Round(0)
}

// Start begins the background tick loop, executing until the context is canceled.
// It compares wall-clock timestamps rather than monotonic clocks because monotonic
// clocks do not advance during system suspend.
func (r *WakeReconciler) Start(ctx context.Context) {
	r.mu.Lock()
	r.lastTick = r.wallTick()
	r.mu.Unlock()

	ticker := time.NewTicker(r.wakeTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wakeCh:
			r.runSweep(ctx)
		case <-ticker.C:
			now := r.wallTick()
			r.mu.Lock()
			last := r.lastTick
			r.lastTick = now
			r.mu.Unlock()
			if now.Sub(last) > r.wakeThreshold {
				r.mu.Lock()
				dirs := append([]watchedDir(nil), r.dirs...)
				r.mu.Unlock()
				slog.Warn("wake_reconciling", "dirs", dirs)
				r.sweepDirs(ctx)
			}
		}
	}
}

// InjectWake triggers a synchronous reconciliation sweep immediately in the current goroutine.
func (r *WakeReconciler) InjectWake() {
	r.runSweep(context.Background())
}

// runSweep executes a synchronous sweep across all registered directories.
func (r *WakeReconciler) runSweep(ctx context.Context) {
	r.sweepDirs(ctx)
}

// sweepDirs compares the modification times, sizes, and prefixes of files in registered directories
// against the baseline store. Sweeps run in parallel up to the concurrency limit.
func (r *WakeReconciler) sweepDirs(ctx context.Context) {
	r.mu.Lock()
	dirs := append([]watchedDir(nil), r.dirs...)
	r.mu.Unlock()

	// Execute the sweep-start hook serially for all repositories before starting parallel file-walks,
	// ensuring initialization tasks are fully completed.
	if r.sweepStart != nil {
		for _, wd := range dirs {
			if ctx.Err() != nil {
				return
			}
			r.sweepStart(ctx, wd.repoID, wd.dir)
		}
	}

	sem := make(chan struct{}, r.concurrency)
	var wg sync.WaitGroup
	for _, wd := range dirs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(wd watchedDir) {
			defer wg.Done()
			defer func() { <-sem }()
			r.sweepOneRepo(ctx, wd.repoID, wd.dir)
		}(wd)
	}
	wg.Wait()

	// Execute the post-sweep hook once all parallel file-walks have completed.
	if r.postSweep != nil && ctx.Err() == nil {
		r.postSweep(ctx)
	}
}

// sweepOneRepo walks the directory tree of a single repository and invokes the change handler.
func (r *WakeReconciler) sweepOneRepo(ctx context.Context, repoID, dir string) {
	r.markReconciling(repoID)
	defer r.clearReconciling(repoID)

	// Resolve the baseline freshly on each sweep to ensure we query the active watcher's baseline.
	store := r.baselineForRepo(repoID)

	r.walkFiles(dir, func(path string, current MtimeEntry) {
		prev, known := store.Get(path)
		store.Put(path, current)

		// Trigger the handler only if a baseline was previously recorded and the file
		// state has changed since that baseline. Initial sightings only record the baseline.
		if known && current.changedFrom(prev) {
			r.handler(ctx, repoID, path)
		}
	})
}

// baselineForRepo resolves the active baseline store for a repository, falling back to the standalone
// store if no resolver is available.
func (r *WakeReconciler) baselineForRepo(repoID string) BaselineStore {
	if r.baselineFor != nil {
		if store, ok := r.baselineFor(repoID); ok && store != nil {
			return store
		}
	}
	return r.standalone
}

// walkFiles walks the directory tree and invokes the callback for each non-ignored file.
func (r *WakeReconciler) walkFiles(dir string, fn func(path string, current MtimeEntry)) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip files matching the ignore patterns, using relative paths where possible.
		rel := path
		if rp, err := filepath.Rel(dir, path); err == nil {
			rel = rp
		}
		if r.ignore != nil && r.ignore.ShouldIgnore(rel) {
			return nil
		}

		current, ok := statEntry(path)
		if !ok {
			return nil
		}
		fn(path, current)
		return nil
	})
}

// statEntry retrieves the modification time, size, and prefix for the specified path.
func statEntry(path string) (MtimeEntry, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return MtimeEntry{}, false
	}
	return MtimeEntry{ModTime: info.ModTime(), Size: info.Size(), Prefix: readPrefix(path)}, true
}

// readPrefix reads the initial bytes of a file up to the prefix length limit, returning a zero-padded buffer.
func readPrefix(path string) [prefixLen]byte {
	var buf [prefixLen]byte
	f, err := os.Open(path)
	if err != nil {
		return buf
	}
	defer f.Close()
	_, _ = io.ReadFull(f, buf[:])
	return buf
}
