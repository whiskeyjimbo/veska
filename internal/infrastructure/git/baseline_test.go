// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFirstWakeReportsOnlySuspendWindowChanges verifies that the reconciler correctly fires
// events only for files modified during a suspend window. When the reconciler shares the
// watcher's baseline, post-suspend wakes only report files modified on disk without an associated
// baseline update, ensuring already-processed live edits do not trigger redundant events.
func TestFirstWakeReportsOnlySuspendWindowChanges(t *testing.T) {
	dir := t.TempDir()
	liveFile := filepath.Join(dir, "live.go")
	suspendFile := filepath.Join(dir, "suspended.go")
	for _, p := range []string{liveFile, suspendFile} {
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fw, err := NewFSWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	fw.seedLastSeen(dir) // Establish the initial session baseline for both files.

	var mu sync.Mutex
	var fired []string
	handler := func(_ context.Context, _, p string) {
		mu.Lock()
		fired = append(fired, p)
		mu.Unlock()
	}

	r := NewWakeReconciler(time.Second, time.Second, handler,
		WithBaseline(func(repoID string) (BaselineStore, bool) { return fw, true }))
	r.AddDir("repo1", dir)

	// Simulate a live edit during the session by changing the file content and updating
	// the baseline store directly. This matches the behavior of a live debounced write.
	if err := os.WriteFile(liveFile, []byte("package x // live edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, ok := statEntry(liveFile)
	if !ok {
		t.Fatal("statEntry(liveFile) failed")
	}
	fw.Put(liveFile, entry)

	// Simulate a suspend window edit where a different file is modified on disk
	// without updating the baseline store.
	if err := os.WriteFile(suspendFile, []byte("package x // changed during suspend\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Trigger the first post-suspend reconciliation wake.
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("first wake fired %d handlers (%v); want exactly 1 (the suspend-window file)", len(fired), fired)
	}
	if fired[0] != suspendFile {
		t.Fatalf("first wake fired for %q; want the suspend-window file %q", fired[0], suspendFile)
	}
}

// TestRefreshBaselineUpdatesToDisk verifies that refreshing the baseline updates the
// entry to match the current on-disk state. This is verified deterministically by calling
// the update function directly rather than relying on fsnotify asynchronous events.
func TestRefreshBaselineUpdatesToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fw, err := NewFSWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()
	fw.seedLastSeen(dir)
	before, _ := fw.Get(path)

	// Modify the file content and explicitly adjust its modification time to ensure the baseline entry changes.
	if err := os.WriteFile(path, []byte("package x // edited longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if !fw.refreshBaseline(context.Background(), path) {
		t.Fatal("refreshBaseline reported no write on a live edit")
	}

	cur, ok := statEntry(path)
	if !ok {
		t.Fatal("statEntry failed")
	}
	got, ok := fw.Get(path)
	if !ok {
		t.Fatal("baseline missing after live write")
	}
	if got.changedFrom(cur) {
		t.Fatal("baseline does not match current on-disk state after live write")
	}
	if !got.changedFrom(before) {
		t.Fatal("baseline did not change after a content+mtime edit")
	}
}

// TestRefreshBaselineSkipsAfterCtxCancel verifies that updates to the baseline are ignored
// if the context has been canceled, preventing stale writes during teardown.
func TestRefreshBaselineSkipsAfterCtxCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fw, err := NewFSWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Mutate the file on disk after cancellation and verify that the baseline is not updated.
	if err := os.WriteFile(path, []byte("package x // after teardown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fw.refreshBaseline(ctx, path) {
		t.Fatal("refreshBaseline wrote the baseline after ctx cancellation; stale-write guard missing")
	}
	if _, ok := fw.Get(path); ok {
		t.Fatal("baseline gained a stale entry after ctx cancellation")
	}
}

// BenchmarkRefreshBaseline measures the latency of updating the file baseline for a recently modified
// file, ensuring that stat operations and small prefix reads do not introduce performance regressions.
func BenchmarkRefreshBaseline(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "hot.go")
	if err := os.WriteFile(path, []byte("package x\nvar _ = 0xDEADBEEF\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	fw, err := NewFSWatcher()
	if err != nil {
		b.Fatal(err)
	}
	defer fw.Close()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fw.refreshBaseline(ctx, path)
	}
}
