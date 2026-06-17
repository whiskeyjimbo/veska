package git

import (
	"context"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const debounceWindow = 50 * time.Millisecond

// FSWatcher implements ports.Watcher using fsnotify with a 50 ms debounce window.
// If an overflow sentinel is received, it executes a fallback walk of the directory
// to detect changed files by comparing their modification time, size, and prefix against
// a last-seen baseline.
type FSWatcher struct {
	mu       sync.Mutex
	fw       *fsnotify.Watcher
	lastSeen map[string]MtimeEntry // guarded by mu; the shared change baseline
	emitFn   func(ports.FileEvent) // set by Watch; used by InjectOverflow
	emitMu   sync.RWMutex          // guards emitFn
}

// NewFSWatcher constructs a new FSWatcher instance and initializes the underlying fsnotify watcher.
func NewFSWatcher() (*FSWatcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &FSWatcher{
		fw:       fw,
		lastSeen: make(map[string]MtimeEntry),
	}, nil
}

// Get retrieves the last-seen baseline entry for the specified file path.
func (w *FSWatcher) Get(path string) (MtimeEntry, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.lastSeen[path]
	return e, ok
}

// Put records the baseline entry for the specified file path.
func (w *FSWatcher) Put(path string, e MtimeEntry) {
	w.mu.Lock()
	w.lastSeen[path] = e
	w.mu.Unlock()
}

// refreshBaseline updates the shared baseline for the specified path to match its current on-disk state.
// It returns false if the context has been cancelled, preventing stale updates after the watcher is stopped.
func (w *FSWatcher) refreshBaseline(ctx context.Context, path string) bool {
	if ctx.Err() != nil {
		return false
	}
	entry, ok := statEntry(path)
	if !ok {
		return false
	}
	w.Put(path, entry)
	return true
}

// Watch registers the directory tree for change events, returning a channel on which file events
// are delivered. The channel is closed when the context is cancelled or Close is called.
func (w *FSWatcher) Watch(ctx context.Context, dir string) (<-chan ports.FileEvent, error) {
	// Recursively add all subdirectories to the watcher.
	if err := w.addTree(dir); err != nil {
		return nil, err
	}

	// Seed the last-seen map to establish an initial baseline for overflow detection.
	w.seedLastSeen(dir)

	out := make(chan ports.FileEvent, 64)

	go w.run(ctx, dir, out)

	return out, nil
}

// setEmit registers the emit callback for synthetic events.
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

// Close stops all directory watches and releases the underlying fsnotify resources.
func (w *FSWatcher) Close() error {
	return w.fw.Close()
}

// InjectOverflow simulates an fsnotify overflow event for testing purposes.
func (w *FSWatcher) InjectOverflow(dir string) {
	w.handleOverflow(w.getEmit(), dir)
}

// addTree walks the directory recursively and adds all subdirectories to the watcher.
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

// seedLastSeen populates the baseline map with current file state details for all files in the directory.
func (w *FSWatcher) seedLastSeen(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		entry, ok := statEntry(path)
		if !ok {
			return nil
		}
		w.Put(path, entry)
		return nil
	})
}

// run executes the main event loop, debouncing writes and forwarding events to the output channel.
func (w *FSWatcher) run(ctx context.Context, dir string, out chan<- ports.FileEvent) {
	// pending maps file paths to their active debounce timers.
	pending := make(map[string]*time.Timer)

	// pendingMu serializes access to the pending timers map.
	var pendingMu sync.Mutex

	// done is closed on exit to notify timer callbacks of shutdown.
	done := make(chan struct{})

	emit := func(ev ports.FileEvent) {
		select {
		case out <- ev:
		case <-done:
		case <-ctx.Done():
		}
	}
	w.setEmit(emit)

	// emitWrite updates the baseline and sends a write event to the output channel.
	emitWrite := func(name string) {
		w.refreshBaseline(ctx, name)
		emit(ports.FileEvent{Path: name, Op: ports.WatchOpWrite})
	}

	// Loop until the context is cancelled or the fsnotify channels close.
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

			// An operation code of zero indicates a kernel event queue overflow.
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
					emitWrite(name)
				})
				pendingMu.Unlock()

			case isRemove:
				// Cancel any pending write debounce timer for the removed path.
				pendingMu.Lock()
				if t, exists := pending[fev.Name]; exists {
					t.Stop()
					delete(pending, fev.Name)
				}
				pendingMu.Unlock()
				emit(ports.FileEvent{Path: fev.Name, Op: ports.WatchOpRemove})

			default:
				// Ignore changes other than writes and removals.
			}
		}
	}

cleanup:
	// Close the done channel to signal in-flight timer callbacks to discard changes.
	close(done)
	w.setEmit(nil)

	// Stop all active timers to prevent post-shutdown executions.
	pendingMu.Lock()
	for path, t := range pending {
		t.Stop()
		delete(pending, path)
	}
	pendingMu.Unlock()

	// Allow in-flight timers that have already expired to terminate cleanly before closing the channel.
	close(out)
}

// handleOverflow performs a full directory scan to reconcile the baseline state and trigger write
// events for any modified files when an overflow occurs.
func (w *FSWatcher) handleOverflow(emit func(ports.FileEvent), dir string) {
	slog.Warn("watcher_overflow", "dir", dir)

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		current, ok := statEntry(path)
		if !ok {
			return nil
		}

		w.mu.Lock()
		prev, known := w.lastSeen[path]
		w.lastSeen[path] = current
		w.mu.Unlock()

		if !known || current.changedFrom(prev) {
			if emit != nil {
				emit(ports.FileEvent{Path: path, Op: ports.WatchOpWrite})
			}
		}
		return nil
	})
}
