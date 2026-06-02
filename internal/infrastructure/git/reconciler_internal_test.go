package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestWallTickStripsMonotonic guards the suspend-detection fix: wallTick must
// return a time with no monotonic reading so gap arithmetic in Start uses
// wall-clock elapsed time (which advances across system suspend) rather than
// monotonic time (which does not). A regression that dropped the .Round(0)
// would let wallTick carry the monotonic component, and got != got.Round(0)
// would then hold. We detect "has monotonic" via Go's == operator, which
// compares the monotonic reading alongside the wall instant.
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

// seedRepoDir creates a dir with one changed file and registers it, seeding the
// reconciler's baseline so a subsequent sweep fires the handler.
func seedRepoDir(t *testing.T, r *WakeReconciler, repoID string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r.AddDir(repoID, dir)
	r.Seed(context.Background())
	// Mutate the file so the next sweep detects a change and fires the handler.
	if err := os.WriteFile(path, []byte("package x // edited\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	return dir
}

// TestSweepStartHook_FiresPerRepoBeforeWalk asserts the sweep-start hook runs
// once per registered repo and that every hook completes BEFORE any file-walk
// handler fires (the serial pre-pass contract).
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
	seedRepoDir(t, r, "repoA")
	seedRepoDir(t, r, "repoB")

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

// TestSweepStartHook_NilSkipped confirms a nil hook leaves the sweep working
// (back-compat: no pre-pass).
func TestSweepStartHook_NilSkipped(t *testing.T) {
	fired := false
	r := NewWakeReconciler(time.Second, time.Second,
		func(_ context.Context, _, _ string) { fired = true })
	seedRepoDir(t, r, "repoA")
	r.InjectWake()
	if !fired {
		t.Error("handler did not fire with a nil sweep-start hook")
	}
}
