package git_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// makeReconciler creates a WakeReconciler with a no-op handler suitable for
// overriding in each test and a fixed nowFn.
func makeReconciler(
	handler git.ReconcileHandler,
	ignore *fs.IgnoreList,
	nowFn func() time.Time,
) *git.WakeReconciler {
	return git.NewWakeReconciler(
		100*time.Millisecond, // wakeTick
		500*time.Millisecond, // wakeThreshold
		handler,
		git.WithIgnoreList(ignore),
		git.WithClock(nowFn),
	)
}

// TestInjectWake_ChangedFile verifies that injectWake calls the handler for a
// file whose mtime was modified between two sweeps.
func TestInjectWake_ChangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.go")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var called []string
	handler := func(_ context.Context, _, p string) {
		mu.Lock()
		called = append(called, p)
		mu.Unlock()
	}

	r := makeReconciler(handler, nil, time.Now)
	r.AddDir("repo1", dir)

	// First injectWake seeds the baseline mtime map.
	r.InjectWake()

	// Advance mtime by touching the file with a future time.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// Second injectWake should detect the change.
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(called) == 0 {
		t.Fatal("expected handler to be called for changed file, but it was not")
	}
	if called[0] != path {
		t.Fatalf("expected handler called with %q, got %q", path, called[0])
	}
}

// TestInjectWake_UnchangedFile verifies the handler is NOT called when files
// have not changed between sweeps.
func TestInjectWake_UnchangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stable.go")
	if err := os.WriteFile(path, []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var called []string
	handler := func(_ context.Context, _, p string) {
		mu.Lock()
		called = append(called, p)
		mu.Unlock()
	}

	r := makeReconciler(handler, nil, time.Now)
	r.AddDir("repo1", dir)

	// Seed baseline.
	r.InjectWake()
	// Second sweep — file unchanged.
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(called) != 0 {
		t.Fatalf("expected no handler calls for unchanged file, got %v", called)
	}
}

// TestIsReconciling verifies that IsReconciling is true during sweep and false
// after sweep completes. We use a blocking handler to hold the sweep in-flight.
func TestIsReconciling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed baseline.
	ready := make(chan struct{})
	proceed := make(chan struct{})

	var r *git.WakeReconciler

	handlerCalled := false
	handler := func(_ context.Context, _, p string) {
		if !handlerCalled {
			handlerCalled = true
			close(ready) // signal that sweep has entered the handler
			<-proceed    // block until test says go
		}
	}

	r = makeReconciler(handler, nil, time.Now)
	r.AddDir("repo1", dir)

	// Seed baseline (first inject — no handler calls expected).
	r.InjectWake()

	// Advance mtime so second sweep fires handler.
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// Run the sweep in a goroutine so we can observe IsReconciling.
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.InjectWake()
	}()

	// Wait until handler is entered.
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to be entered")
	}

	if !r.IsReconciling() {
		t.Error("expected IsReconciling() == true during sweep")
	}

	// Let the sweep complete.
	close(proceed)
	<-done

	if r.IsReconciling() {
		t.Error("expected IsReconciling() == false after sweep")
	}
}

// TestIgnoredFile verifies that files matched by the IgnoreList are not
// passed to the handler.
func TestIgnoredFile(t *testing.T) {
	dir := t.TempDir()

	// Create a normal file and an ignored file.
	normalPath := filepath.Join(dir, "main.go")
	ignoredPath := filepath.Join(dir, "main_gen.go")
	for _, p := range []string{normalPath, ignoredPath} {
		if err := os.WriteFile(p, []byte("pkg"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var called []string
	handler := func(_ context.Context, _, p string) {
		mu.Lock()
		called = append(called, p)
		mu.Unlock()
	}

	// Build an ignore list that matches *_gen.go files.
	il := fs.NewIgnoreListFromPatterns([]string{"*_gen.go"})

	r := makeReconciler(handler, il, time.Now)
	r.AddDir("repo1", dir)

	// Seed baseline.
	r.InjectWake()

	// Advance mtime on both files.
	future := time.Now().Add(2 * time.Second)
	for _, p := range []string{normalPath, ignoredPath} {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}

	// Second sweep — only normalPath should fire.
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	for _, c := range called {
		if c == ignoredPath {
			t.Errorf("ignored file %q was passed to handler", ignoredPath)
		}
	}
	found := false
	for _, c := range called {
		if c == normalPath {
			found = true
		}
	}
	if !found {
		t.Error("expected normal file to be reported but it was not")
	}
}

// TestFreezeMonotonicClock_WakeDetected verifies the "freeze monotonic clock"
// scenario: nowFn returns a time far in the future after the first tick,
// simulating a suspend/resume gap. Changed files must be detected.
func TestFreezeMonotonicClock_WakeDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.go")
	if err := os.WriteFile(path, []byte("wake"), 0o644); err != nil {
		t.Fatal(err)
	}

	// nowFn advances time dramatically on the second call to simulate a
	// wall-clock gap larger than wakeThreshold (500 ms).
	var callCount int
	var nowMu sync.Mutex
	base := time.Now()
	nowFn := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		callCount++
		if callCount <= 1 {
			return base
		}
		// Return a time 10 seconds ahead — well past wakeThreshold.
		return base.Add(10 * time.Second)
	}

	var mu sync.Mutex
	var called []string
	handler := func(_ context.Context, _, p string) {
		mu.Lock()
		called = append(called, p)
		mu.Unlock()
	}

	r := git.NewWakeReconciler(
		100*time.Millisecond,
		500*time.Millisecond,
		handler,
		git.WithClock(nowFn),
	)
	r.AddDir("repo1", dir)

	// First InjectWake seeds baseline (nowFn call 1 = base time).
	r.InjectWake()

	// Advance file mtime so sweep detects a change.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// Second InjectWake: nowFn call 2 returns base+10s (simulated wake).
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(called) == 0 {
		t.Fatal("expected handler called after simulated wake, got nothing")
	}
	if called[0] != path {
		t.Fatalf("expected handler called with %q, got %q", path, called[0])
	}
}

// TestStart_WakeGapTriggersSweep drives the real tick loop (not InjectWake):
// a clock that jumps forward past wakeThreshold between ticks must trigger a
// sweep that reports the changed file with its owning repo ID.
func TestStart_WakeGapTriggersSweep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The first clock read (Start's baseline lastTick) returns base; every
	// later read jumps 10s ahead, so the first ticker read shows a gap well
	// past the 500ms threshold the helper sets — i.e. a simulated resume.
	base := time.Now()
	var nowMu sync.Mutex
	var calls int
	nowFn := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		calls++
		if calls <= 1 {
			return base
		}
		return base.Add(10 * time.Second)
	}

	type hit struct{ repoID, path string }
	hits := make(chan hit, 4)
	handler := func(_ context.Context, repoID, p string) {
		hits <- hit{repoID, p}
	}

	r := git.NewWakeReconciler(20*time.Millisecond, 500*time.Millisecond, handler, git.WithClock(nowFn))
	r.AddDir("repoA", dir)
	// Seed the mtime baseline (no nowFn call) so the post-gap sweep sees a real
	// change, then bump mtime so the gap sweep has something to report.
	r.InjectWake()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Start(ctx)

	select {
	case h := <-hits:
		if h.repoID != "repoA" || h.path != path {
			t.Fatalf("got hit %+v, want repoID=repoA path=%s", h, path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tick loop did not sweep after simulated wake gap")
	}
}

// TestStart_ContextCancel verifies that Start exits cleanly when ctx is cancelled.
func TestStart_ContextCancel(t *testing.T) {
	r := makeReconciler(func(context.Context, string, string) {}, nil, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Start(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after ctx cancellation")
	}
}
