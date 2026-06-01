package git

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	infrafs "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
)

// MtimeEntry records the last-seen mtime and size of a file for change detection.
type MtimeEntry struct {
	ModTime time.Time
	Size    int64
}

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

// WakeReconciler detects system suspend/resume by comparing wall-clock time
// across a periodic tick interval. When the gap between two ticks exceeds
// wakeThreshold, it sweeps the registered directory trees for mtime/size changes
// and calls the handler for each changed file.
type WakeReconciler struct {
	wakeTick      time.Duration
	wakeThreshold time.Duration
	handler       ReconcileHandler
	ignore        *infrafs.IgnoreList
	nowFn         func() time.Time

	mu          sync.Mutex
	dirs        []watchedDir
	mtimeMap    map[string]MtimeEntry // guarded by mu
	lastTick    time.Time             // guarded by mu
	reconciling atomic.Bool

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

// WithIgnoreList supplies a .gitignore-semantics matcher; changed files it
// matches are skipped. A nil list (the default) skips nothing.
func WithIgnoreList(ignore *infrafs.IgnoreList) Option {
	return func(r *WakeReconciler) { r.ignore = ignore }
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
		mtimeMap:      make(map[string]MtimeEntry),
		wakeCh:        make(chan struct{}, 1),
	}
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

// IsReconciling returns true while a wake sweep is in progress.
func (r *WakeReconciler) IsReconciling() bool {
	return r.reconciling.Load()
}

// wallTick returns the current time with its monotonic reading stripped, so
// gap arithmetic in Start measures wall-clock elapsed time (which advances
// across system suspend) rather than monotonic time (which does not).
func (r *WakeReconciler) wallTick() time.Time {
	return r.nowFn().Round(0)
}

// Start begins the background tick loop. Stops when ctx is cancelled.
//
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

// runSweep performs the mtime sweep synchronously.
func (r *WakeReconciler) runSweep(ctx context.Context) {
	r.reconciling.Store(true)
	defer r.reconciling.Store(false)
	r.sweepDirs(ctx)
}

// sweepDirs walks each registered directory, compares mtime+size to the last-seen
// map, calls handler for changed files, and updates the map.
func (r *WakeReconciler) sweepDirs(ctx context.Context) {
	r.mu.Lock()
	dirs := append([]watchedDir(nil), r.dirs...)
	r.mu.Unlock()

	for _, wd := range dirs {
		r.sweepDir(ctx, wd.repoID, wd.dir)
	}
}

func (r *WakeReconciler) sweepDir(ctx context.Context, repoID, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip ignored files. The matcher uses .gitignore semantics and
		// expects a path relative to the swept root, so anchored patterns
		// resolve correctly; fall back to the absolute path if Rel fails.
		rel := path
		if rp, err := filepath.Rel(dir, path); err == nil {
			rel = rp
		}
		if r.ignore != nil && r.ignore.ShouldIgnore(rel) {
			return nil
		}

		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}

		current := MtimeEntry{ModTime: info.ModTime(), Size: info.Size()}

		r.mu.Lock()
		prev, known := r.mtimeMap[path]
		r.mtimeMap[path] = current
		r.mu.Unlock()

		if !known || prev.ModTime != current.ModTime || prev.Size != current.Size {
			if known {
				// Only call handler when we have a baseline (second+ sweep).
				r.handler(ctx, repoID, path)
			}
		}
		return nil
	})
}
