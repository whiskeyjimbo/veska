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

	infrafs "github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/fs"
)

// MtimeEntry records the last-seen mtime and size of a file for change detection.
type MtimeEntry struct {
	ModTime time.Time
	Size    int64
}

// ReconcileHandler is called with the path of each file whose mtime/size changed
// since last sweep. The reconciler calls this for every changed file it discovers.
type ReconcileHandler func(path string)

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
	dirs        []string
	mtimeMap    map[string]MtimeEntry // guarded by mu
	lastTick    time.Time             // guarded by mu
	reconciling atomic.Bool

	// wakeCh is used by injectWake to trigger an immediate sweep.
	wakeCh chan struct{}
}

// NewWakeReconciler creates a reconciler that ticks every wakeTick and considers
// a gap > wakeThreshold to indicate a suspend/resume event.
// handler is called with the absolute path of each changed file on wake.
// ignore may be nil (no files are skipped).
// nowFn is injectable for testing (use time.Now in production).
func NewWakeReconciler(
	wakeTick time.Duration,
	wakeThreshold time.Duration,
	handler ReconcileHandler,
	ignore *infrafs.IgnoreList,
	nowFn func() time.Time,
) *WakeReconciler {
	return &WakeReconciler{
		wakeTick:      wakeTick,
		wakeThreshold: wakeThreshold,
		handler:       handler,
		ignore:        ignore,
		nowFn:         nowFn,
		mtimeMap:      make(map[string]MtimeEntry),
		wakeCh:        make(chan struct{}, 1),
	}
}

// AddDir registers a directory tree to sweep on wake.
func (r *WakeReconciler) AddDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirs = append(r.dirs, dir)
}

// IsReconciling returns true while a wake sweep is in progress.
func (r *WakeReconciler) IsReconciling() bool {
	return r.reconciling.Load()
}

// Start begins the background tick loop. Stops when ctx is cancelled.
func (r *WakeReconciler) Start(ctx context.Context) {
	r.mu.Lock()
	r.lastTick = r.nowFn()
	r.mu.Unlock()

	ticker := time.NewTicker(r.wakeTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wakeCh:
			r.runSweep()
		case <-ticker.C:
			now := r.nowFn()
			r.mu.Lock()
			last := r.lastTick
			r.lastTick = now
			r.mu.Unlock()
			if now.Sub(last) > r.wakeThreshold {
				r.mu.Lock()
				dirs := append([]string(nil), r.dirs...)
				r.mu.Unlock()
				slog.Warn("wake_reconciling", "dirs", dirs)
				r.sweepDirs()
			}
		}
	}
}

// InjectWake simulates a wake event for testing. It runs the sweep
// synchronously in the caller's goroutine so tests can observe results
// immediately after the call returns.
func (r *WakeReconciler) InjectWake() {
	r.runSweep()
}

// runSweep performs the mtime sweep synchronously.
func (r *WakeReconciler) runSweep() {
	r.reconciling.Store(true)
	defer r.reconciling.Store(false)
	r.sweepDirs()
}

// sweepDirs walks each registered directory, compares mtime+size to the last-seen
// map, calls handler for changed files, and updates the map.
func (r *WakeReconciler) sweepDirs() {
	r.mu.Lock()
	dirs := append([]string(nil), r.dirs...)
	r.mu.Unlock()

	for _, dir := range dirs {
		r.sweepDir(dir)
	}
}

func (r *WakeReconciler) sweepDir(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		// Skip ignored files.
		if r.ignore != nil && r.ignore.ShouldIgnore(path) {
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
				r.handler(path)
			}
		}
		return nil
	})
}
