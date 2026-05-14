package git_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
