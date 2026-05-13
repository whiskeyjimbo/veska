package git_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/ports"
	"github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/git"
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

// TestFSWatcher_Write verifies that writing to a watched file emits WatchOpWrite.
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

	// Create and write a file.
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

// TestFSWatcher_Remove verifies that removing a file emits WatchOpRemove.
func TestFSWatcher_Remove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Pre-create the file so we're removing an existing file.
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

	// Drain any create events from the initial watch registration.
	time.Sleep(100 * time.Millisecond)
	// drain buffered
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

// TestFSWatcher_Debounce verifies that two rapid writes produce only one FileEvent.
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
	// Write twice within 10ms — well inside the 50ms debounce window.
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}

	// Collect all events for 200ms (4x debounce window).
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

// TestFSWatcher_OverflowFallback verifies that injecting an overflow emits
// "watcher_overflow" in logs and emits FileEvents for changed files.
func TestFSWatcher_OverflowFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Capture slog output.
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

	// Create a file so there's something to detect in the overflow walk.
	path := filepath.Join(dir, "overflow_file.go")
	if err := os.WriteFile(path, []byte("initial"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Drain the initial write event (if any) so we have a clean baseline.
	time.Sleep(150 * time.Millisecond)
	for {
		select {
		case <-ch:
		default:
			goto cleanBaseline
		}
	}
cleanBaseline:

	// Modify the file to change mtime/size, then inject overflow.
	if err := os.WriteFile(path, []byte("changed content after overflow"), 0o644); err != nil {
		t.Fatalf("WriteFile changed: %v", err)
	}

	// Inject the overflow — this should walk the dir, compare stats, and emit events.
	w.InjectOverflow(dir)

	// Wait for event.
	ev, ok := firstEvent(ch, eventTimeout)
	if !ok {
		t.Fatal("expected FileEvent from overflow polling, got none")
	}
	if ev.Op != ports.WatchOpWrite {
		t.Errorf("expected WatchOpWrite from overflow, got %q", ev.Op)
	}

	// Check that "watcher_overflow" appeared in the log.
	logOutput := buf.String()
	if !bytes.Contains([]byte(logOutput), []byte("watcher_overflow")) {
		t.Errorf("expected 'watcher_overflow' in slog output, got: %s", logOutput)
	}
}
