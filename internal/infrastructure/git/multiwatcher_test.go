// SPDX-License-Identifier: AGPL-3.0-only

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

// TestMultiRepoWatcherAdd verifies that new repositories can be added and the events channel is initialized.
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

// TestMultiRepoWatcherAddBeforeStart guards against a repo-add that races
// ahead of Start (m.ctx still nil) panicking on context.WithCancel(nil).
// The watcher falls back to a background context and stays functional.
func TestMultiRepoWatcherAddBeforeStart(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	// Deliberately skip Start() to reproduce the boot race window.
	dir := t.TempDir()

	if err := mw.Add("repoEarly", dir); err != nil {
		t.Fatalf("Add before Start: %v", err)
	}

	ids := mw.WatchedRepoIDs()
	if len(ids) != 1 || ids[0] != "repoEarly" {
		t.Fatalf("WatchedRepoIDs after early Add = %v, want [repoEarly]", ids)
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherRemove verifies that repositories can be successfully removed without errors or panics.
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

	// Removing a non-existent repository should return an error rather than causing a panic.
	err := mw.Remove("doesNotExist")
	if err == nil {
		t.Error("expected error when removing non-existent repo, got nil")
	}

	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMultiRepoWatcherReceivesEvent verifies that writing a file to a watched directory triggers a corresponding event.
func TestMultiRepoWatcherReceivesEvent(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	ctx := t.Context()
	mw.Start(ctx)

	dir := t.TempDir()

	if err := mw.Add("repoY", dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Wait briefly to allow the file watcher to initialize.
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

// TestMultiRepoWatcherIsolatedDebounce verifies that file events in one watched repository do not leak into another.
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

	// Wait briefly to allow both file watchers to register.
	time.Sleep(50 * time.Millisecond)

	// Modify a file only within repository A's directory.
	pathA := filepath.Join(dirA, "change.go")
	if err := os.WriteFile(pathA, []byte("package a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Collect events for a duration exceeding the debounce interval.
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

// TestMultiRepoWatcherRestartAll verifies that restarting all file watchers successfully
// teardown and recreates watcher instances, and that subsequent file changes are still detected.
func TestMultiRepoWatcherRestartAll(t *testing.T) {
	t.Parallel()

	mw := git.NewMultiRepoWatcher()
	mw.Start(t.Context())

	dir := t.TempDir()
	if err := mw.Add("repoR", dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	mw.RestartAll()

	// The repository must remain in the watched set after the restart.
	ids := mw.WatchedRepoIDs()
	if len(ids) != 1 || ids[0] != "repoR" {
		t.Fatalf("WatchedRepoIDs after restart = %v, want [repoR]", ids)
	}

	// Wait briefly for the new watcher handles to register before modifying the file.
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

// TestMultiRepoWatcherInject verifies that manually injecting file events successfully
// pipes them to the events channel, and that injection before starting does not panic.
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
