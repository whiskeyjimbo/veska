package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFirstWakeReportsOnlySuspendWindowChanges is the behavioral heart of
// It proves the convergence goal: when the reconciler shares
// the watcher's live baseline (kept current by the live save path), the first
// post-suspend wake fires the handler ONLY for files edited during the suspend
// window — NOT for files edited live during the session whose baseline already
// tracks disk.
// Setup mirrors the production wiring without fsnotify timing flake:
//
//	a real FSWatcher is the BaselineStore, wired via WithBaseline;
//	seedLastSeen records the baseline (as Watch does);
//	the LIVE edit goes through the watcher's baseline Put (what the live
//	  debounced-write path does), so its baseline == current disk → no fire;
//	the SUSPEND-WINDOW edit changes a DIFFERENT file on disk WITHOUT updating
//	  the watcher baseline (as a real suspend does) → fires.
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
	fw.seedLastSeen(dir) // establish the session-start baseline for both files

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

	// LIVE edit during the session: change content, then update the shared
	// baseline through the watcher's Put — exactly what emitWrite does on a live
	// debounced write. Its baseline now equals disk.
	if err := os.WriteFile(liveFile, []byte("package x // live edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, ok := statEntry(liveFile)
	if !ok {
		t.Fatal("statEntry(liveFile) failed")
	}
	fw.Put(liveFile, entry)

	// SUSPEND-WINDOW edit: a DIFFERENT file changes on disk while the watcher is
	// suspended, so its baseline is NOT updated.
	if err := os.WriteFile(suspendFile, []byte("package x // changed during suspend\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First post-suspend wake.
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

// TestRefreshBaselineUpdatesToDisk proves the LIVE debounced-write action
// (refreshBaseline, called by the run loop on every debounced write) updates the
// shared baseline to current on-disk state (mtime+size+prefix). Tested
// deterministically by calling the extracted method directly rather than relying
// on fsnotify timing. This is the code change that makes the behavioral test
// above hold in production.
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

	// Change content AND force a distinct mtime so the entry must differ.
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

// TestRefreshBaselineSkipsAfterCtxCancel proves the teardown-race guard
// (guardrail 3): a debounced timer that fires AFTER its FSWatcher's ctx is
// cancelled (i.e. after RestartAll tore it down) does NOT write to the baseline.
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

	// Mutate disk after cancel; refreshBaseline must refuse to record it.
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

// BenchmarkRefreshBaseline measures the live-emit baseline update on a
// page-cache-hot just-written file (guardrail 2: hot-path no-regression
// evidence). It is the per-debounced-write cost the convergence adds: one
// os.Stat + a 64-byte prefix read + a map Put under a mutex.
// Measured (Intel i7-7700, Linux, t.TempDir page-cache hot):
//
//	BenchmarkRefreshBaseline-4 ~5985 ns/op, 504 B/op, 6 allocs/op.
//
// Cost argument: ~6µs runs once per debounced write, AFTER the 50ms debounce
// window and BEFORE a channel send + downstream tree-sitter parse + Ingester
// Save. Six microseconds of stat + hot 64-byte read is ~4 orders of magnitude
// below the 50ms debounce it follows and is dwarfed by the parse it precedes, so
// it is not a measurable hot-path regression.
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
