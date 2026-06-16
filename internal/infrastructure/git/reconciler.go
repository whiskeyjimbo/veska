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

// prefixLen is the number of leading bytes the wake sweep compares in addition
// to mtime+size. Format-on-save tools (gofmt, prettier, ruff) routinely produce
// same-length replacements, and some filesystems coalesce mtime at second
// granularity — so a file edited during suspend can pass the (mtime, size)
// check unchanged. Comparing the first 64 bytes is a cheap intermediate that
// catches most such edits; full content hashing is too expensive for working
// trees with >50k files. Edits beyond the first 64 bytes that
// keep mtime+size still slip through and are caught at the next live save or
// promotion diff.
const prefixLen = 64

// MtimeEntry records the last-seen mtime, size, and leading-byte prefix of a
// file for change detection.
type MtimeEntry struct {
	ModTime time.Time
	Size    int64
	Prefix  [prefixLen]byte
}

// changedFrom reports whether e differs from prev in mtime, size, or prefix.
func (e MtimeEntry) changedFrom(prev MtimeEntry) bool {
	return e.ModTime != prev.ModTime || e.Size != prev.Size || e.Prefix != prev.Prefix
}

// SweepStartHook is invoked once per registered repo at the start of a wake
// sweep, SERIALLY and BEFORE any parallel file-walk begins. It carries the
// owning repo ID and root directory. The daemon wires the staging-vs-HEAD
// branch reconcile here; the reconciler itself stays infra-pure and just calls
// the callback (no staging/application import). ctx is the sweep's context.
type SweepStartHook func(ctx context.Context, repoID, dir string)

// PostSweepHook is invoked exactly once at the end of a wake sweep, AFTER every
// per-repo file-walk has joined. The daemon wires the watcher handle-restart
// here: live events resume against a fresh OS stream once the
// mtime sweep has covered the suspend window. The reconciler stays infra-pure
// it just calls the callback (no watcher/application import). ctx is the sweep's
// context; the hook is skipped if the sweep returns early on cancellation.
type PostSweepHook func(ctx context.Context)

// ReconcileHandler is called with the owning repo ID and the path of each file
// whose mtime/size changed since the last sweep. The reconciler calls this for
// every changed file it discovers. ctx is the sweep's context so a long handler
// can honour daemon shutdown.
type ReconcileHandler func(ctx context.Context, repoID, path string)

// watchedDir pairs a registered directory tree with the repo that owns it, so
// the handler can be told which repo a changed file belongs to.
type watchedDir struct {
	repoID string
	dir    string
}

// BaselineStore is the per-file change-detection baseline a wake sweep compares
// against. *FSWatcher satisfies it (its live lastSeen map), so the reconciler
// converges onto the watcher's continuously-updated baseline rather than a
// separate seeded copy. Implementations must be safe for
// concurrent use — a parallel per-repo sweep and live debounced writes both
// touch the store.
type BaselineStore interface {
	Get(path string) (MtimeEntry, bool)
	Put(path string, e MtimeEntry)
}

// BaselineResolver returns the CURRENT BaselineStore for a repo, resolved FRESH
// on each sweep. The daemon wires it to the MultiRepoWatcher so a sweep follows
// RestartAll replacing a repo's FSWatcher: each sweep reads
// the live FSWatcher's baseline, not a pointer captured at construction. ok is
// false when the repo is not (yet) watched, in which case the reconciler falls
// back to its standalone in-memory baseline.
type BaselineResolver func(repoID string) (BaselineStore, bool)

// memBaseline is the standalone in-memory baseline used when no BaselineResolver
// is wired (the reconciler running without a watcher: eval harness and unit
// tests). It is filled lazily by the sweep's first sighting of each file — NOT
// by a separate seed pass — so "first sighting records, fires nothing" still
// holds. This is the seam's standalone implementation, not a revival of the
// retired per-reconciler mtimeMap.
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

// WakeReconciler detects system suspend/resume by comparing wall-clock time
// across a periodic tick interval. When the gap between two ticks exceeds
// wakeThreshold, it sweeps the registered directory trees for mtime/size changes
// and calls the handler for each changed file.
type WakeReconciler struct {
	wakeTick      time.Duration
	wakeThreshold time.Duration
	handler       ReconcileHandler
	sweepStart    SweepStartHook
	postSweep     PostSweepHook
	ignore        *infrafs.IgnoreList
	nowFn         func() time.Time
	// concurrency bounds how many per-repo sweeps run in parallel. Always
	// resolved to a positive value at construction (NumCPU/2, floor 1).
	concurrency int

	// baselineFor resolves the live BaselineStore for a repo, fresh each sweep
	// (so it tracks RestartAll replacing the FSWatcher). Nil / a false return
	// falls back to the standalone in-memory baseline below. Set via
	// WithBaseline.
	baselineFor BaselineResolver
	// standalone is the fallback baseline when baselineFor is unset or the repo
	// is not watched. It keeps the reconciler usable without a watcher (eval
	// harness, unit tests).
	standalone *memBaseline

	mu       sync.Mutex
	dirs     []watchedDir
	lastTick time.Time // guarded by mu
	// reconciling is the set of repo IDs whose per-repo sweep is in flight,
	// guarded by mu. Per-repo (not a global flag) so an MCP query against a
	// settled repo is not flagged wake_reconciling because some OTHER repo is
	// still sweeping.
	reconciling map[string]int

	// wakeCh is used by injectWake to trigger an immediate sweep.
	wakeCh chan struct{}
}

// Option configures a WakeReconciler at construction.
type Option func(*WakeReconciler)

// WithClock overrides the time source (default time.Now). Injectable for tests.
func WithClock(nowFn func() time.Time) Option {
	return func(r *WakeReconciler) {
		if nowFn != nil {
			r.nowFn = nowFn
		}
	}
}

// WithIgnoreList supplies a.gitignore-semantics matcher; changed files it
// matches are skipped. A nil list (the default) skips nothing.
func WithIgnoreList(ignore *infrafs.IgnoreList) Option {
	return func(r *WakeReconciler) { r.ignore = ignore }
}

// WithSweepStartHook registers a callback invoked once per registered repo at
// the start of a wake sweep, serially and before the parallel file-walk phase
// begins (so all generation bumps complete before any parse runs). A nil hook
// (the default) skips the pre-pass.
func WithSweepStartHook(fn SweepStartHook) Option {
	return func(r *WakeReconciler) { r.sweepStart = fn }
}

// WithPostSweepHook registers a callback invoked exactly once at the end of a
// wake sweep, after every per-repo file-walk has joined. The daemon wires the
// watcher handle-restart here. A nil hook (the default) skips the after-phase. A
// sweep that returns early on context cancellation does NOT invoke the hook
// that correctly avoids restarting watcher handles during shutdown.
func WithPostSweepHook(fn PostSweepHook) Option {
	return func(r *WakeReconciler) { r.postSweep = fn }
}

// WithBaseline wires the resolver that supplies the live per-repo change
// baseline (the watcher's lastSeen). When unset, the reconciler uses a
// standalone in-memory baseline filled lazily by the sweep (first-sighting
// records, fires nothing) — keeping it usable without a watcher.
func WithBaseline(fn BaselineResolver) Option {
	return func(r *WakeReconciler) { r.baselineFor = fn }
}

// WithWakeConcurrency caps how many per-repo sweeps run in parallel on a wake
// event. n <= 0 (the default) resolves to runtime.NumCPU/2 with a floor of 1.
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

// NewWakeReconciler creates a reconciler that ticks every wakeTick and considers
// a gap > wakeThreshold to indicate a suspend/resume event. handler is called
// with the owning repo ID and absolute path of each changed file on wake.
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

// AddDir registers a repo's directory tree to sweep on wake.
func (r *WakeReconciler) AddDir(repoID, dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirs = append(r.dirs, watchedDir{repoID: repoID, dir: dir})
}

// IsRepoReconciling reports whether the given repo's wake sweep is in flight.
func (r *WakeReconciler) IsRepoReconciling(repoID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconciling[repoID] > 0
}

// ReconcilingRepos returns the sorted set of repo IDs whose wake sweep is
// currently in flight. Empty when no sweep is running.
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

// markReconciling records that repoID's sweep has started.
func (r *WakeReconciler) markReconciling(repoID string) {
	r.mu.Lock()
	r.reconciling[repoID]++
	r.mu.Unlock()
}

// clearReconciling records that repoID's sweep has finished.
func (r *WakeReconciler) clearReconciling(repoID string) {
	r.mu.Lock()
	if r.reconciling[repoID] <= 1 {
		delete(r.reconciling, repoID)
	} else {
		r.reconciling[repoID]--
	}
	r.mu.Unlock()
}

// wallTick returns the current time with its monotonic reading stripped, so
// gap arithmetic in Start measures wall-clock elapsed time (which advances
// across system suspend) rather than monotonic time (which does not).
func (r *WakeReconciler) wallTick() time.Time {
	return r.nowFn().Round(0)
}

// Start begins the background tick loop. Stops when ctx is cancelled.
// Gap detection compares wall-clock readings, not monotonic ones. time.Time's
// Sub uses the monotonic component when present, and CLOCK_MONOTONIC (Linux) /
// mach_absolute_time (macOS) do NOT advance while the system is suspended — so
// a monotonic comparison would see only ~wakeTick after a real sleep and never
// fire. wallTick strips the monotonic reading (.Round(0)) so Sub falls back to
// wall-clock arithmetic, which does advance across suspend. This is the whole
// point of the detector.
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

// InjectWake simulates a wake event for testing. It runs the sweep
// synchronously in the caller's goroutine so tests can observe results
// immediately after the call returns.
func (r *WakeReconciler) InjectWake() {
	r.runSweep(context.Background())
}

// runSweep performs the mtime sweep synchronously (blocks until every per-repo
// sweep finishes), then returns.
func (r *WakeReconciler) runSweep(ctx context.Context) {
	r.sweepDirs(ctx)
}

// sweepDirs walks each registered directory tree, comparing mtime+size to the
// last-seen map and calling the handler for changed files. Per-repo sweeps run
// in parallel, bounded by r.concurrency. The handler may be invoked from
// multiple goroutines concurrently.
func (r *WakeReconciler) sweepDirs(ctx context.Context) {
	r.mu.Lock()
	dirs := append([]watchedDir(nil), r.dirs...)
	r.mu.Unlock()

	// Serial pre-pass: run the sweep-start hook for EVERY repo before launching
	// any parallel file-walk, so all staging generation bumps complete before
	// any parse runs (: "bumped *before* any parse runs"). Running
	// it serially also avoids a parallel branch bump spuriously invalidating
	// another repo's concurrently-starting parse.
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

	// After-phase: restart the watcher handles so live events resume against a
	// fresh stream. Runs once, after all parallel walks join. Skipped on a
	// cancelled context so shutdown does not recreate watchers we are tearing
	// down.
	if r.postSweep != nil && ctx.Err() == nil {
		r.postSweep(ctx)
	}
}

// sweepOneRepo walks one repo's directory tree and fires the handler for each
// changed file. The repo is marked reconciling for the duration so an MCP query
// against it can observe the in-flight sweep (cleared via defer on return).
func (r *WakeReconciler) sweepOneRepo(ctx context.Context, repoID, dir string) {
	r.markReconciling(repoID)
	defer r.clearReconciling(repoID)

	// Resolve the baseline FRESH per sweep so we follow RestartAll replacing the
	// repo's FSWatcher: a sweep started after a restart reads the new watcher's
	// live baseline, never a stale captured pointer ( /.25.3).
	store := r.baselineForRepo(repoID)

	r.walkFiles(dir, func(path string, current MtimeEntry) {
		prev, known := store.Get(path)
		store.Put(path, current)

		// Fire only for a file we already had a baseline for (live edit, prior
		// sweep, or watcher seed) that has since changed. A first-ever sighting
		// just records state. Because the live save path keeps the watcher
		// baseline current, a file edited during the session but NOT during the
		// suspend window has prev == current and does not fire — so the first
		// post-suspend wake reports only suspend-window changes.
		if known && current.changedFrom(prev) {
			r.handler(ctx, repoID, path)
		}
	})
}

// baselineForRepo resolves the live BaselineStore for repoID via the wired
// resolver, falling back to the standalone in-memory baseline when no resolver
// is set or the repo is not (yet) watched.
func (r *WakeReconciler) baselineForRepo(repoID string) BaselineStore {
	if r.baselineFor != nil {
		if store, ok := r.baselineFor(repoID); ok && store != nil {
			return store
		}
	}
	return r.standalone
}

// walkFiles walks dir and invokes fn with the current MtimeEntry for each
// non-ignored regular file. Stat/ignore handling is shared across sweeps.
func (r *WakeReconciler) walkFiles(dir string, fn func(path string, current MtimeEntry)) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip ignored files. The matcher uses.gitignore semantics and
		// expects a path relative to the swept root, so anchored patterns
		// resolve correctly; fall back to the absolute path if Rel fails.
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

// statEntry reads the mtime, size, and leading-byte prefix of path. ok is false
// when the file cannot be stat'd (e.g. removed mid-walk), in which case the
// caller skips it.
func statEntry(path string) (MtimeEntry, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return MtimeEntry{}, false
	}
	return MtimeEntry{ModTime: info.ModTime(), Size: info.Size(), Prefix: readPrefix(path)}, true
}

// readPrefix returns the first prefixLen bytes of path, zero-padded for shorter
// files. A read error yields the zero prefix — change detection still falls
// back to mtime+size, so a transient open failure cannot wedge the sweep.
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
