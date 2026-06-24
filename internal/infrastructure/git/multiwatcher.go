// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RepoFileEvent wraps a ports.FileEvent with the source repository identifier.
type RepoFileEvent struct {
	RepoID string
	Event  ports.FileEvent
}

// repoWatch tracks the FSWatcher and context cancellation function for a specific repository.
type repoWatch struct {
	repoID   string
	rootPath string
	watcher  *FSWatcher
	cancel   context.CancelFunc
}

// MultiRepoWatcher manages a dynamic collection of repository-specific FSWatcher instances,
// each maintaining an isolated debounce queue. Events from all watched repositories are multiplexed
// onto a single output channel. If a watcher goroutine encounters a panic, it recovers, logs the
// error, and restarts the watcher for that repository after a one-second delay.
type MultiRepoWatcher struct {
	mu     sync.RWMutex
	repos  map[string]*repoWatch
	out    chan RepoFileEvent
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once // ensures out is only closed once
}

// NewMultiRepoWatcher constructs a new MultiRepoWatcher instance. Start must be called before adding repositories.
func NewMultiRepoWatcher() *MultiRepoWatcher {
	return &MultiRepoWatcher{
		repos: make(map[string]*repoWatch),
		out:   make(chan RepoFileEvent, 256),
	}
}

// Start initializes the multiplexer's parent context. This must be called before calling Add. Canceling the context is equivalent to calling Close.
func (m *MultiRepoWatcher) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
}

// Events returns the channel on which file events from all watched repositories are delivered.
func (m *MultiRepoWatcher) Events() <-chan RepoFileEvent {
	return m.out
}

// WatchedRepoIDs returns the identifiers of all repositories currently being watched in an unspecified order. This method is safe for concurrent access.
func (m *MultiRepoWatcher) WatchedRepoIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.repos))
	for id := range m.repos {
		ids = append(ids, id)
	}
	return ids
}

// Add starts watching the specified root path for a repository. It creates a dedicated FSWatcher
// and launches a forwarding goroutine to multiplex events. If the forwarding goroutine panics, it
// recovers, logs the error, and automatically restarts the watcher after one second.
func (m *MultiRepoWatcher) Add(repoID, rootPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.repos[repoID]; exists {
		// Already watching - idempotent.
		return nil
	}

	rw, err := m.startRepoWatch(repoID, rootPath)
	if err != nil {
		return err
	}

	m.repos[repoID] = rw
	return nil
}

// startRepoWatch constructs and launches a new repository watcher. The caller must hold the write lock.
func (m *MultiRepoWatcher) startRepoWatch(repoID, rootPath string) (*repoWatch, error) {
	fw, err := NewFSWatcher()
	if err != nil {
		return nil, err
	}

	// Fall back to Background if Add races ahead of Start before m.ctx is wired
	// (mirrors repoRegistrar.AddRepo's daemonCtx guard). The daemon ordering
	// makes this unreachable in production, so a hit signals a regression - warn
	// loudly rather than silently watch on a detached context (solov2-bihz).
	parent := m.ctx
	if parent == nil {
		slog.Warn("multiwatcher: Add before Start - watching on background context", "repoID", repoID)
		parent = context.Background()
	}
	repoCtx, repoCancel := context.WithCancel(parent)

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

// forwardEvents reads events from the source channel and forwards them to the shared output channel.
// It intercepts panics, logging the error and initiating a restart of the repository watcher after a delay.
func (m *MultiRepoWatcher) forwardEvents(ctx context.Context, rw *repoWatch, eventCh <-chan ports.FileEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("multiwatcher: panic in repo watcher goroutine",
				"repoID", rw.repoID,
				"panic", r,
			)
			// Drain remaining events to prevent any goroutine from blocking on a channel send.
			go func() {
				for range eventCh { //nolint:revive
				}
			}()

			// Restart the watcher after one second unless the parent context has been canceled.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(time.Second):
			}

			m.mu.Lock()
			// Only restart the watcher if the repository remains in the registered set.
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
				// The channel was closed, indicating a normal shutdown.
				return
			}
			select {
			case m.out <- RepoFileEvent{RepoID: rw.repoID, Event: ev}:
			case <-m.ctx.Done():
				// Drain remaining events to allow the FSWatcher goroutine to terminate cleanly.
				go func() {
					for range eventCh { //nolint:revive
					}
				}()
				return
			}
		case <-ctx.Done():
			// Drain remaining events to allow the FSWatcher goroutine to terminate cleanly.
			go func() {
				for range eventCh { //nolint:revive
				}
			}()
			return
		}
	}
}

// Inject synthesizes a write event for a file path and sends it to the shared output channel.
// This is used to pipe suspend-detected changes into the parse-on-save pipeline. The send is
// bounded by the multiplexer context to prevent blocked consumers from causing deadlocks on shutdown.
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

// RestartAll tears down and recreates the FSWatcher for every watched repository, establishing
// fresh operating system file handles. This method is called after a suspend/resume reconciliation
// sweep to resume live event tracking. Recreation failures are logged and the corresponding entry is
// removed to prevent stalling other repositories.
func (m *MultiRepoWatcher) RestartAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for repoID, rw := range m.repos {
		rw.cancel()
		_ = rw.watcher.Close()

		newRW, err := m.startRepoWatch(repoID, rw.rootPath)
		if err != nil {
			slog.Error("multiwatcher: failed to restart repo watcher on wake",
				"repoID", repoID,
				"err", err,
			)
			delete(m.repos, repoID)
			continue
		}
		m.repos[repoID] = newRW
	}
}

// BaselineFor returns the live change-detection baseline for a repository under a read lock.
// It returns false if the repository is not currently being watched, indicating that the reconciler
// should fall back to a standalone baseline.
func (m *MultiRepoWatcher) BaselineFor(repoID string) (BaselineStore, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rw, ok := m.repos[repoID]
	if !ok {
		return nil, false
	}
	return rw.watcher, true
}

// Remove stops the watcher for repoID and removes it from the tracked set.
// It returns an error if the repository is not found in the watched set.
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

// Close stops all repository watchers and closes the multiplexed events channel.
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

	// Close the output channel exactly once to prevent panics.
	m.once.Do(func() {
		close(m.out)
	})

	return nil
}
