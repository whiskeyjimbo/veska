package git_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

const multiEventTimeout = 500 * time.Millisecond

// TestMultiRepoWatcherAdd creates a MultiRepoWatcher, adds two tempdir repos,
// and verifies that the Events() channel is non-nil.
func TestMultiRepoWatcherAdd(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	ctx := t.Context()
	mw.Start(ctx)

	dirA := t.TempDir()
	dirB := t.TempDir()

	if err := mw.Add("repoA", dirA); err != nil {
		t.Fatalf("Add repoA: %v", err)
	}
	if err := mw.Add("repoB", dirB); err != nil {
		t.Fatalf("Add repoB: %v", err)
	}

	ch := mw.Events()
	if ch == nil {
		t.Fatal("Events() returned nil channel")
		return
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherRemove adds then removes a repo and verifies no panic and
// that Remove returns nil.
func TestMultiRepoWatcherRemove(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	ctx := t.Context()
	mw.Start(ctx)

	dir := t.TempDir()

	if err := mw.Add("repoX", dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := mw.Remove("repoX"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Remove a non-existent repo should return an error, not panic.
	err := mw.Remove("doesNotExist")
	if err == nil {
		t.Error("expected error when removing non-existent repo, got nil")
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherReceivesEvent adds a tempdir, starts the watcher, creates a
// file, and expects a RepoFileEvent on Events() within 500ms.
func TestMultiRepoWatcherReceivesEvent(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	ctx := t.Context()
	mw.Start(ctx)

	dir := t.TempDir()

	if err := mw.Add("repoY", dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Small sleep to allow the watcher to register before writing.
	time.Sleep(50 * time.Millisecond)

	path := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case ev, ok := <-mw.Events():
		if !ok {
			t.Fatal("Events() channel closed unexpectedly")
		}
		if ev.RepoID != "repoY" {
			t.Errorf("expected RepoID %q, got %q", "repoY", ev.RepoID)
		}
	case <-time.After(multiEventTimeout):
		t.Fatal("timed out waiting for RepoFileEvent")
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherIsolatedDebounce verifies that writing to repo A produces
// only repo A's ID in the events, not repo B's.
func TestMultiRepoWatcherIsolatedDebounce(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	ctx := t.Context()
	mw.Start(ctx)

	dirA := t.TempDir()
	dirB := t.TempDir()

	if err := mw.Add("alpha", dirA); err != nil {
		t.Fatalf("Add alpha: %v", err)
	}
	if err := mw.Add("beta", dirB); err != nil {
		t.Fatalf("Add beta: %v", err)
	}

	// Small sleep to allow both watchers to register.
	time.Sleep(50 * time.Millisecond)

	// Write only to repo A.
	pathA := filepath.Join(dirA, "change.go")
	if err := os.WriteFile(pathA, []byte("package a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Collect events for 300ms (well past the 50ms debounce).
	deadline := time.After(300 * time.Millisecond)
	var repoIDs []string
loop:
	for {
		select {
		case ev, ok := <-mw.Events():
			if !ok {
				break loop
			}
			repoIDs = append(repoIDs, ev.RepoID)
		case <-deadline:
			break loop
		}
	}

	if len(repoIDs) == 0 {
		t.Fatal("expected at least one event from alpha, got none")
	}
	for _, id := range repoIDs {
		if id != "alpha" {
			t.Errorf("expected only events from alpha, got %q", id)
		}
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherRestartAll tears down and recreates each repo's FSWatcher
// (the wake-handle restart, solov2-xde2.25.3) and verifies that a live file
// write AFTER the restart still produces a RepoFileEvent — proving the fresh
// handle is wired to the same Events() stream. No file is written before the
// restart, so the only event that can arrive is the post-restart one (the
// buffered out channel cannot replay a stale pre-restart event as a false pass).
func TestMultiRepoWatcherRestartAll(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	mw.Start(t.Context())

	dir := t.TempDir()
	if err := mw.Add("repoR", dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	mw.RestartAll()

	// The repo must still be watched after the restart.
	ids := mw.WatchedRepoIDs()
	if len(ids) != 1 || ids[0] != "repoR" {
		t.Fatalf("WatchedRepoIDs after restart = %v, want [repoR]", ids)
	}

	// Let the fresh watcher register its inotify watches before writing.
	time.Sleep(50 * time.Millisecond)

	path := filepath.Join(dir, "post-wake.go")
	if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case ev, ok := <-mw.Events():
		if !ok {
			t.Fatal("Events() channel closed unexpectedly")
		}
		if ev.RepoID != "repoR" {
			t.Errorf("RepoID = %q, want repoR", ev.RepoID)
		}
	case <-time.After(multiEventTimeout):
		t.Fatal("timed out waiting for post-restart RepoFileEvent (fresh handle not live)")
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherInject verifies that Inject multiplexes a synthetic write
// event (the wake-reconciler path) onto Events() with the given repo ID, and
// that an Inject before Start is a no-op rather than a panic.
func TestMultiRepoWatcherInject(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	mw.Inject("early", "/tmp/before-start.go") // must not panic before Start

	mw.Start(t.Context())

	mw.Inject("repoZ", "/tmp/woke.go")

	select {
	case ev := <-mw.Events():
		if ev.RepoID != "repoZ" {
			t.Errorf("RepoID = %q, want repoZ", ev.RepoID)
		}
		if ev.Event.Path != "/tmp/woke.go" {
			t.Errorf("Path = %q, want /tmp/woke.go", ev.Event.Path)
		}
		if ev.Event.Op != ports.WatchOpWrite {
			t.Errorf("Op = %q, want write", ev.Event.Op)
		}
	case <-time.After(multiEventTimeout):
		t.Fatal("no injected event delivered")
	}
}
