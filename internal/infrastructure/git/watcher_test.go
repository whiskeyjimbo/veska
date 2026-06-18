// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package git_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

const eventTimeout = 2 * time.Second

func drainEvents(ch <-chan ports.FileEvent, timeout time.Duration) []ports.FileEvent {
	var events []ports.FileEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			return events
		}
	}
}

func firstEvent(ch <-chan ports.FileEvent, timeout time.Duration) (ports.FileEvent, bool) {
	select {
	case ev, ok := <-ch:
		if !ok {
			return ports.FileEvent{}, false
		}
		return ev, true
	case <-time.After(timeout):
		return ports.FileEvent{}, false
	}
}

// TestFSWatcher_Write verifies that writing to a watched file emits a WatchOpWrite event.
func TestFSWatcher_Write(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := git.NewFSWatcher()
	if err != nil {
		t.Fatalf("NewFSWatcher: %v", err)
	}
	defer w.Close()

	ctx := t.Context()

	ch, err := w.Watch(ctx, dir)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Create and write content to a test file.
	path := filepath.Join(dir, "test.go")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev, ok := firstEvent(ch, eventTimeout)
	if !ok {
		t.Fatal("expected FileEvent for write, got none")
	}
	if ev.Op != ports.WatchOpWrite {
		t.Errorf("expected WatchOpWrite, got %q", ev.Op)
	}
	if ev.Path != path {
		t.Errorf("expected path %q, got %q", path, ev.Path)
	}
}

// TestFSWatcher_Remove verifies that removing a file emits a WatchOpRemove event.
func TestFSWatcher_Remove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Pre-create a file so that we have an active tracked file to delete.
	path := filepath.Join(dir, "remove_me.go")
	if err := os.WriteFile(path, []byte("bye"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w, err := git.NewFSWatcher()
	if err != nil {
		t.Fatalf("NewFSWatcher: %v", err)
	}
	defer w.Close()

	ctx := t.Context()

	ch, err := w.Watch(ctx, dir)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Drain any file events triggered during the initial watch registration.
	time.Sleep(100 * time.Millisecond)
	// Drain all remaining events from the buffer.
	for {
		select {
		case <-ch:
		default:
			goto drained
		}
	}
drained:

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	ev, ok := firstEvent(ch, eventTimeout)
	if !ok {
		t.Fatal("expected FileEvent for remove, got none")
	}
	if ev.Op != ports.WatchOpRemove {
		t.Errorf("expected WatchOpRemove, got %q", ev.Op)
	}
}

// TestFSWatcher_Debounce verifies that multiple rapid writes to the same file are debounced into a single file event.
func TestFSWatcher_Debounce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	w, err := git.NewFSWatcher()
	if err != nil {
		t.Fatalf("NewFSWatcher: %v", err)
	}
	defer w.Close()

	ctx := t.Context()

	ch, err := w.Watch(ctx, dir)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	path := filepath.Join(dir, "debounced.go")
	// Perform two writes within a brief interval to check the debounce logic.
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}

	// Collect all events for a duration that allows the debounce timer to settle.
	events := drainEvents(ch, 200*time.Millisecond)

	writeCount := 0
	for _, ev := range events {
		if ev.Path == path && ev.Op == ports.WatchOpWrite {
			writeCount++
		}
	}
	if writeCount != 1 {
		t.Errorf("expected exactly 1 debounced write event for %q, got %d (events: %v)", path, writeCount, events)
	}
}

// TestFSWatcher_OverflowFallback verifies that an injected overflow event is handled correctly
// by logging the overflow warning and scanning the directory for changes.
func TestFSWatcher_OverflowFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Redirect default structured logging to a buffer for verification.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	w, err := git.NewFSWatcher()
	if err != nil {
		t.Fatalf("NewFSWatcher: %v", err)
	}
	defer w.Close()

	ctx := t.Context()

	ch, err := w.Watch(ctx, dir)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Create an initial file to establish a baseline in the watch tree.
	path := filepath.Join(dir, "overflow_file.go")
	if err := os.WriteFile(path, []byte("initial"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Drain any initial file events to ensure a clean starting point.
	time.Sleep(150 * time.Millisecond)
	for {
		select {
		case <-ch:
		default:
			goto cleanBaseline
		}
	}
cleanBaseline:

	// Modify the file content to alter its signature, then trigger the overflow simulation.
	if err := os.WriteFile(path, []byte("changed content after overflow"), 0o644); err != nil {
		t.Fatalf("WriteFile changed: %v", err)
	}

	// Inject the overflow event to trigger a full directory scan.
	w.InjectOverflow(dir)

	// Wait for the expected write event to be delivered.
	ev, ok := firstEvent(ch, eventTimeout)
	if !ok {
		t.Fatal("expected FileEvent from overflow polling, got none")
	}
	if ev.Op != ports.WatchOpWrite {
		t.Errorf("expected WatchOpWrite from overflow, got %q", ev.Op)
	}

	// Verify that the overflow warning was logged.
	logOutput := buf.String()
	if !bytes.Contains([]byte(logOutput), []byte("watcher_overflow")) {
		t.Errorf("expected 'watcher_overflow' in slog output, got: %s", logOutput)
	}
}
