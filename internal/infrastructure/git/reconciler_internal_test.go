package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWallTickStripsMonotonic verifies that wallTick strips the monotonic clock component.
// This is critical for gap arithmetic across system suspends because monotonic clocks do
// not advance while the system is suspended.
func TestWallTickStripsMonotonic(t *testing.T) {
	mono := time.Now() // time.Now always carries a monotonic reading
	r := NewWakeReconciler(
		time.Second, time.Second,
		func(context.Context, string, string) {},
		WithClock(func() time.Time { return mono }),
	)

	got := r.wallTick()
	if got != got.Round(0) {
		t.Fatal("wallTick returned a time that still carries a monotonic reading")
	}
	if !got.Equal(mono) {
		t.Fatalf("wallTick changed the wall instant: got %v, want %v", got, mono)
	}
}

// registerRepoDir creates a directory containing a single file, registers it, and returns the file path.
func registerRepoDir(t *testing.T, r *WakeReconciler, repoID string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r.AddDir(repoID, dir)
	return path
}

// seedThenMutate initializes the standalone baseline directly for the specified paths,
// then mutates each file on disk to guarantee that the next sweep detects changes.
func seedThenMutate(t *testing.T, r *WakeReconciler, paths ...string) {
	t.Helper()
	for _, path := range paths {
		entry, ok := statEntry(path)
		if !ok {
			t.Fatalf("statEntry(%s) failed during seed", path)
		}
		r.standalone.Put(path, entry)
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("package x // edited\n"), 0o644); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
	}
}

// TestSweepStartHook_FiresPerRepoBeforeWalk asserts that the sweep-start hook runs
// once per registered repository and completes before any file walks occur.
func TestSweepStartHook_FiresPerRepoBeforeWalk(t *testing.T) {
	var mu sync.Mutex
	hookCount := map[string]int{}
	handlerSeen := false
	hookAfterHandler := false

	handler := func(_ context.Context, repoID, _ string) {
		mu.Lock()
		handlerSeen = true
		mu.Unlock()
	}
	hook := func(_ context.Context, repoID, _ string) {
		mu.Lock()
		hookCount[repoID]++
		if handlerSeen {
			hookAfterHandler = true
		}
		mu.Unlock()
	}

	r := NewWakeReconciler(time.Second, time.Second, handler,
		WithWakeConcurrency(4), WithSweepStartHook(hook))
	pA := registerRepoDir(t, r, "repoA")
	pB := registerRepoDir(t, r, "repoB")
	seedThenMutate(t, r, pA, pB)

	r.InjectWake()

	if hookAfterHandler {
		t.Error("a sweep-start hook fired AFTER a file-walk handler; pre-pass not serial-before-walk")
	}
	if hookCount["repoA"] != 1 || hookCount["repoB"] != 1 {
		t.Errorf("hook fire counts = %v, want each repo exactly 1", hookCount)
	}
	if !handlerSeen {
		t.Error("file-walk handler never fired; test setup did not produce a changed file")
	}
}

// TestPostSweepHook_FiresOnceAfterWalk asserts that the post-sweep hook runs
// exactly once after all file walks have completed.
func TestPostSweepHook_FiresOnceAfterWalk(t *testing.T) {
	var mu sync.Mutex
	postCount := 0
	handlerSeen := false
	postBeforeHandler := false

	handler := func(_ context.Context, _, _ string) {
		mu.Lock()
		handlerSeen = true
		mu.Unlock()
	}
	post := func(_ context.Context) {
		mu.Lock()
		postCount++
		if !handlerSeen {
			postBeforeHandler = true
		}
		mu.Unlock()
	}

	r := NewWakeReconciler(time.Second, time.Second, handler,
		WithWakeConcurrency(4), WithPostSweepHook(post))
	pA := registerRepoDir(t, r, "repoA")
	pB := registerRepoDir(t, r, "repoB")
	seedThenMutate(t, r, pA, pB)

	r.InjectWake()

	if postBeforeHandler {
		t.Error("post-sweep hook fired BEFORE a file-walk handler; not after-phase")
	}
	if postCount != 1 {
		t.Errorf("post-sweep hook fired %d times, want exactly 1", postCount)
	}
	if !handlerSeen {
		t.Error("file-walk handler never fired; test setup did not produce a changed file")
	}
}

// TestPostSweepHook_NilSkipped confirms that a nil post-sweep hook does not affect sweep functionality.
func TestPostSweepHook_NilSkipped(t *testing.T) {
	fired := false
	r := NewWakeReconciler(time.Second, time.Second,
		func(_ context.Context, _, _ string) { fired = true })
	pA := registerRepoDir(t, r, "repoA")
	seedThenMutate(t, r, pA)
	r.InjectWake()
	if !fired {
		t.Error("handler did not fire with a nil post-sweep hook")
	}
}

// TestSweepStartHook_NilSkipped confirms that a nil sweep-start hook does not affect sweep functionality.
func TestSweepStartHook_NilSkipped(t *testing.T) {
	fired := false
	r := NewWakeReconciler(time.Second, time.Second,
		func(_ context.Context, _, _ string) { fired = true })
	pA := registerRepoDir(t, r, "repoA")
	seedThenMutate(t, r, pA)
	r.InjectWake()
	if !fired {
		t.Error("handler did not fire with a nil sweep-start hook")
	}
}
