package git

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RepoFileEvent wraps a ports.FileEvent with the source repo ID.
type RepoFileEvent struct {
	RepoID string
	Event  ports.FileEvent
}

// repoWatch holds the per-repo watcher state.
type repoWatch struct {
	repoID   string
	rootPath string
	watcher  *FSWatcher
	cancel   context.CancelFunc
}

// MultiRepoWatcher manages a dynamic set of per-repo FSWatcher instances.
// Each repo has its own isolated debounce queue. Events from all repos are
// multiplexed onto a single channel returned by Events().
// A panic in one repo's watcher goroutine is recovered, logged, and that
// repo is restarted after a 1-second delay.
type MultiRepoWatcher struct {
	mu     sync.RWMutex
	repos  map[string]*repoWatch
	out    chan RepoFileEvent
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once // ensures out is only closed once
}

// NewMultiRepoWatcher creates a new MultiRepoWatcher. Call Start before Add.
func NewMultiRepoWatcher() *MultiRepoWatcher {
	return &MultiRepoWatcher{
		repos: make(map[string]*repoWatch),
		out:   make(chan RepoFileEvent, 256),
	}
}

// Start initialises the multiplexer's parent context. It must be called before
// any Add calls. Cancelling ctx is equivalent to calling Close.
func (m *MultiRepoWatcher) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
}

// Events returns the channel on which RepoFileEvents from all watched repos
// are delivered.
func (m *MultiRepoWatcher) Events() <-chan RepoFileEvent {
	return m.out
}

// WatchedRepoIDs returns the IDs of every repository currently being watched.
// The order is unspecified. It is safe to call concurrently with Add/Remove.
func (m *MultiRepoWatcher) WatchedRepoIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.repos))
	for id := range m.repos {
		ids = append(ids, id)
	}
	return ids
}

// Add starts watching rootPath for repoID. A per-repo FSWatcher is created and
// a forwarding goroutine is launched that multiplexes its events onto the shared
// output channel. If the forwarding goroutine panics it is recovered, logged,
// and the watcher is restarted after 1 second.
func (m *MultiRepoWatcher) Add(repoID, rootPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.repos[repoID]; exists {
		// Already watching — idempotent.
		return nil
	}

	rw, err := m.startRepoWatch(repoID, rootPath)
	if err != nil {
		return err
	}

	m.repos[repoID] = rw
	return nil
}

// startRepoWatch creates and starts a new repoWatch. The caller must hold mu (or
// it must be called before sharing). It is also used by the panic-recovery path.
func (m *MultiRepoWatcher) startRepoWatch(repoID, rootPath string) (*repoWatch, error) {
	fw, err := NewFSWatcher()
	if err != nil {
		return nil, err
	}

	repoCtx, repoCancel := context.WithCancel(m.ctx)

	eventCh, err := fw.Watch(repoCtx, rootPath)
	if err != nil {
		repoCancel()
		_ = fw.Close()
		return nil, err
	}

	rw := &repoWatch{
		repoID:   repoID,
		rootPath: rootPath,
		watcher:  fw,
		cancel:   repoCancel,
	}

	go m.forwardEvents(repoCtx, rw, eventCh)

	return rw, nil
}

// forwardEvents reads from eventCh and forwards each event to the shared output
// channel with the repo ID attached. It recovers from panics and restarts the
// watcher for the affected repo after 1 second.
func (m *MultiRepoWatcher) forwardEvents(ctx context.Context, rw *repoWatch, eventCh <-chan ports.FileEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("multiwatcher: panic in repo watcher goroutine",
				"repoID", rw.repoID,
				"panic", r,
			)
			// Drain remaining events to avoid a goroutine blocked on send.
			go func() {
				for range eventCh { //nolint:revive
				}
			}()

			// Restart after 1 second unless the parent context is already done.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(time.Second):
			}

			m.mu.Lock()
			// Only restart if the repo is still registered (Remove may have been called).
			if _, exists := m.repos[rw.repoID]; exists {
				newRW, err := m.startRepoWatch(rw.repoID, rw.rootPath)
				if err != nil {
					slog.Error("multiwatcher: failed to restart repo watcher",
						"repoID", rw.repoID,
						"err", err,
					)
				} else {
					m.repos[rw.repoID] = newRW
				}
			}
			m.mu.Unlock()
		}
	}()

	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				// Channel closed — normal shutdown.
				return
			}
			select {
			case m.out <- RepoFileEvent{RepoID: rw.repoID, Event: ev}:
			case <-m.ctx.Done():
				// Drain remaining events so the FSWatcher goroutine can exit.
				go func() {
					for range eventCh { //nolint:revive
					}
				}()
				return
			}
		case <-ctx.Done():
			// Drain remaining events so the FSWatcher goroutine can exit.
			go func() {
				for range eventCh { //nolint:revive
				}
			}()
			return
		}
	}
}

// Inject synthesises a write event for path under repoID and multiplexes it
// onto the shared output channel, exactly as a live fsnotify write would. The
// wake reconciler uses this to feed suspend/resume-detected changes into the
// same parse-on-save pipeline that drains Events(). The send is bounded by the
// multiplexer context so a stalled consumer cannot block a wake sweep past
// shutdown. Calling before Start (m.ctx unset) is a no-op.
func (m *MultiRepoWatcher) Inject(repoID, path string) {
	if m.ctx == nil {
		return
	}
	ev := RepoFileEvent{RepoID: repoID, Event: ports.FileEvent{Path: path, Op: ports.WatchOpWrite}}
	select {
	case m.out <- ev:
	case <-m.ctx.Done():
	}
}

// Remove stops the watcher for repoID and removes it from the tracked set.
// Returns an error if repoID is not currently watched.
func (m *MultiRepoWatcher) Remove(repoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rw, exists := m.repos[repoID]
	if !exists {
		return errors.New("multiwatcher: repo not found: " + repoID)
	}

	rw.cancel()
	_ = rw.watcher.Close()
	delete(m.repos, repoID)
	return nil
}

// Close stops all repo watchers and closes the Events() channel.
func (m *MultiRepoWatcher) Close() error {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	for id, rw := range m.repos {
		rw.cancel()
		_ = rw.watcher.Close()
		delete(m.repos, id)
	}
	m.mu.Unlock()

	// Close the output channel exactly once.
	m.once.Do(func() {
		close(m.out)
	})

	return nil
}
