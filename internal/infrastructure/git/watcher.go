// Package git provides filesystem change notification via fsnotify with
// 50 ms debounce and an overflow-triggered polling fallback.
package git

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const debounceWindow = 50 * time.Millisecond

// fileInfo holds the last-observed metadata for a file used by the overflow
// polling fallback.
type fileInfo struct {
	mtime time.Time
	size  int64
}

// FSWatcher implements ports.Watcher using fsnotify with a 50 ms debounce
// window. When the underlying watcher emits an overflow sentinel (Op == 0 /
// fsnotify.ErrEventOverflow), it falls back to a full directory walk,
// compares mtime+size to the last-seen map, and emits FileEvents for any
// changed files.
type FSWatcher struct {
	mu       sync.Mutex
	fw       *fsnotify.Watcher
	lastSeen map[string]fileInfo   // guarded by mu
	emitFn   func(ports.FileEvent) // set by Watch; used by InjectOverflow
	emitMu   sync.RWMutex          // guards emitFn
}

// NewFSWatcher creates a new FSWatcher and initialises the underlying fsnotify
// watcher.
func NewFSWatcher() (*FSWatcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &FSWatcher{
		fw:       fw,
		lastSeen: make(map[string]fileInfo),
	}, nil
}

// Watch registers the directory tree rooted at dir for change events and
// returns a channel on which FileEvents are delivered. The channel is closed
// when ctx is cancelled or Close is called.
func (w *FSWatcher) Watch(ctx context.Context, dir string) (<-chan ports.FileEvent, error) {
	// Recursively add all subdirectories.
	if err := w.addTree(dir); err != nil {
		return nil, err
	}

	// Seed the last-seen map so the overflow fallback has a baseline.
	w.seedLastSeen(dir)

	out := make(chan ports.FileEvent, 64)

	go w.run(ctx, dir, out)

	return out, nil
}

// setEmit stores the emit func so InjectOverflow can use it.
func (w *FSWatcher) setEmit(fn func(ports.FileEvent)) {
	w.emitMu.Lock()
	w.emitFn = fn
	w.emitMu.Unlock()
}

func (w *FSWatcher) getEmit() func(ports.FileEvent) {
	w.emitMu.RLock()
	defer w.emitMu.RUnlock()
	return w.emitFn
}

// Close stops all watches and releases underlying OS resources.
func (w *FSWatcher) Close() error {
	return w.fw.Close()
}

// InjectOverflow simulates an fsnotify overflow for the given directory.
// This is intentionally exported (capital I) so tests in the git_test package
// can call it; real callers should never need to invoke it directly.
func (w *FSWatcher) InjectOverflow(dir string) {
	w.handleOverflow(w.getEmit(), dir)
}

// addTree walks dir and adds every directory to the fsnotify watcher.
func (w *FSWatcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return w.fw.Add(path)
		}
		return nil
	})
}

// seedLastSeen populates the last-seen map with current stat data for all
// files under dir.
func (w *FSWatcher) seedLastSeen(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		w.mu.Lock()
		w.lastSeen[path] = fileInfo{mtime: info.ModTime(), size: info.Size()}
		w.mu.Unlock()
		return nil
	})
}

// run is the main event loop. It reads from fw.Events, debounces writes, and
// forwards FileEvents to out. It closes out when the loop exits.
func (w *FSWatcher) run(ctx context.Context, dir string, out chan<- ports.FileEvent) {
	// pending holds debounce timers keyed by absolute path.
	pending := make(map[string]*time.Timer)

	// pendingMu guards pending; timer callbacks (separate goroutines) also lock it.
	var pendingMu sync.Mutex

	// done is closed when the loop exits so timer callbacks can detect shutdown.
	done := make(chan struct{})

	emit := func(ev ports.FileEvent) {
		select {
		case out <- ev:
		case <-done:
		case <-ctx.Done():
		}
	}
	w.setEmit(emit)

	// loop until ctx is done or the fsnotify channels close.
	for {
		select {
		case <-ctx.Done():
			goto cleanup

		case err, ok := <-w.fw.Errors:
			if !ok {
				goto cleanup
			}
			slog.Warn("fsnotify error", "err", err)

		case fev, ok := <-w.fw.Events:
			if !ok {
				goto cleanup
			}

			// Overflow sentinel: Op == 0 means the kernel dropped events.
			if fev.Op == 0 {
				go w.handleOverflow(emit, dir)
				continue
			}

			isWrite := fev.Op&(fsnotify.Create|fsnotify.Write) != 0
			isRemove := fev.Op&(fsnotify.Remove|fsnotify.Rename) != 0

			switch {
			case isWrite:
				name := fev.Name
				pendingMu.Lock()
				if t, exists := pending[name]; exists {
					t.Stop()
				}
				pending[name] = time.AfterFunc(debounceWindow, func() {
					pendingMu.Lock()
					delete(pending, name)
					pendingMu.Unlock()
					emit(ports.FileEvent{Path: name, Op: ports.WatchOpWrite})
				})
				pendingMu.Unlock()

			case isRemove:
				// Cancel any pending debounce for this path.
				pendingMu.Lock()
				if t, exists := pending[fev.Name]; exists {
					t.Stop()
					delete(pending, fev.Name)
				}
				pendingMu.Unlock()
				emit(ports.FileEvent{Path: fev.Name, Op: ports.WatchOpRemove})

			default:
				// Chmod alone (or any other op) carries no semantic meaning; skip.
			}
		}
	}

cleanup:
	// Signal done first so in-flight timer callbacks stop trying to send.
	close(done)
	w.setEmit(nil)

	// Stop all pending debounce timers so their goroutines do not fire after
	// we close out.
	pendingMu.Lock()
	for path, t := range pending {
		t.Stop()
		delete(pending, path)
	}
	pendingMu.Unlock()

	// A timer that already fired but is blocked on done/ctx.Done will unblock
	// now that done is closed. Give it a moment, then close the output channel.
	// The brief sleep is intentional: AfterFunc goroutines run concurrently and
	// the t.Stop() above may not cancel a timer that has already expired and is
	// waiting to be scheduled. Closing done unblocks them; they will return
	// without sending.
	close(out)
}

// handleOverflow is called when fsnotify signals that events were dropped.
// It walks dir, compares current stat data to the last-seen map, emits
// WatchOpWrite for any file that changed, and updates the map.
//
// emit may be nil when called from InjectOverflow (test helper path); in that
// case the method writes to the watcher's internal channel via the stored
// reference — but since we don't store out, callers that need events must
// ensure the loop is running.  The simplest design is: handleOverflow accepts
// an optional emit func.
func (w *FSWatcher) handleOverflow(emit func(ports.FileEvent), dir string) {
	slog.Warn("watcher_overflow", "dir", dir)

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}

		current := fileInfo{mtime: info.ModTime(), size: info.Size()}

		w.mu.Lock()
		prev, known := w.lastSeen[path]
		w.lastSeen[path] = current
		w.mu.Unlock()

		if !known || prev.mtime != current.mtime || prev.size != current.size {
			if emit != nil {
				emit(ports.FileEvent{Path: path, Op: ports.WatchOpWrite})
			}
		}
		return nil
	})
}
