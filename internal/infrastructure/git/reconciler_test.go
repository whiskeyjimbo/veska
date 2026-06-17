package git_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// makeReconciler constructs a WakeReconciler helper with a customizable handler and a clock function.
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

// TestInjectWake_ChangedFile verifies that InjectWake triggers the handler when a file's modification time changes between sweeps.
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

	// The first InjectWake call seeds the initial baseline.
	r.InjectWake()

	// Simulate an edit by updating the modification time to a future timestamp.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// The second InjectWake call should detect the updated file.
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

// TestInjectWake_UnchangedFile verifies that the handler is not triggered when files remain unchanged between sweeps.
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

	// Seed the baseline store.
	r.InjectWake()
	// Execute a second sweep where the file is unchanged.
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(called) != 0 {
		t.Fatalf("expected no handler calls for unchanged file, got %v", called)
	}
}

// TestIsRepoReconciling_PerRepo verifies that only the repository whose sweep is actively running
// reports true, and that the flag is correctly cleared once the sweep is completed.
func TestIsRepoReconciling_PerRepo(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	pathA := filepath.Join(dirA, "f.go")
	pathB := filepath.Join(dirB, "f.go")
	if err := os.WriteFile(pathA, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	proceed := make(chan struct{})

	var once sync.Once
	handler := func(_ context.Context, repoID, _ string) {
		if repoID == "repoA" {
			once.Do(func() {
				close(ready) // Close the channel once the sweep handler for repoA is entered.
				<-proceed    // Block the handler to hold the sweep in progress.
			})
		}
	}

	// Configure concurrency to 1 so that repoB does not sweep while repoA is blocked,
	// allowing us to verify that repoB is not marked as reconciling.
	r := git.NewWakeReconciler(100*time.Millisecond, 500*time.Millisecond, handler,
		git.WithClock(time.Now), git.WithWakeConcurrency(1))
	r.AddDir("repoA", dirA)
	r.AddDir("repoB", dirB)

	// Seed the baseline using a first sweep which should not invoke the handler.
	r.InjectWake()

	// Modify repoA's file modification time to trigger the handler on the next sweep.
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(pathA, future, future); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.InjectWake()
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for repoA handler to be entered")
	}

	if !r.IsRepoReconciling("repoA") {
		t.Error("expected IsRepoReconciling(repoA) == true during repoA sweep")
	}
	if r.IsRepoReconciling("repoB") {
		t.Error("expected IsRepoReconciling(repoB) == false while only repoA sweeps")
	}
	if got := r.ReconcilingRepos(); len(got) != 1 || got[0] != "repoA" {
		t.Errorf("expected ReconcilingRepos == [repoA]; got %v", got)
	}

	close(proceed)
	<-done

	if r.IsRepoReconciling("repoA") {
		t.Error("expected IsRepoReconciling(repoA) == false after sweep")
	}
	if got := r.ReconcilingRepos(); len(got) != 0 {
		t.Errorf("expected no reconciling repos after sweep; got %v", got)
	}
}

// TestSweepConcurrencyCap verifies that parallel repository sweeps do not exceed the configured concurrency limit.
func TestSweepConcurrencyCap(t *testing.T) {
	const repos = 6
	const cap = 2

	var r *git.WakeReconciler
	dirs := make([]string, repos)
	for i := range dirs {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "f.go"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs[i] = d
	}

	var mu sync.Mutex
	active, maxActive := 0, 0
	release := make(chan struct{})
	entered := make(chan struct{}, repos) // one token per handler entry

	handler := func(_ context.Context, _, _ string) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release // Block execution until the test releases the barrier.
		mu.Lock()
		active--
		mu.Unlock()
	}

	r = git.NewWakeReconciler(100*time.Millisecond, 500*time.Millisecond, handler,
		git.WithClock(time.Now), git.WithWakeConcurrency(cap))
	for i, d := range dirs {
		r.AddDir(repoID(i), d)
	}

	// Seed the initial baseline.
	r.InjectWake()

	// Update modification times for all files so they register as modified.
	future := time.Now().Add(3 * time.Second)
	for _, d := range dirs {
		if err := os.Chtimes(filepath.Join(d, "f.go"), future, future); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.InjectWake()
	}()

	// Wait until `cap` handlers have entered (proving parallelism reached the
	// Wait until the concurrent sweeps have all blocked at the cap limit.
	for range cap {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for cap concurrent sweeps")
		}
	}

	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for sweep to finish")
	}

	mu.Lock()
	got := maxActive
	mu.Unlock()
	if got > cap {
		t.Errorf("max concurrent sweeps = %d, exceeds cap %d", got, cap)
	}
	if got < cap {
		t.Errorf("max concurrent sweeps = %d, expected to reach cap %d (no parallelism?)", got, cap)
	}
}

func repoID(i int) string { return "repo" + string(rune('A'+i)) }

// TestIgnoredFile verifies that files matched by the ignore patterns do not trigger the handler.
func TestIgnoredFile(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file and an ignored file.
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

	// Configure ignore patterns to match generated files.
	il := fs.NewIgnoreListFromPatterns([]string{"*_gen.go"})

	r := makeReconciler(handler, il, time.Now)
	r.AddDir("repo1", dir)

	// Seed the baseline store.
	r.InjectWake()

	// Update modification times on both files.
	future := time.Now().Add(2 * time.Second)
	for _, p := range []string{normalPath, ignoredPath} {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}

	// Perform a second sweep and verify only the non-ignored file triggers the handler.
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

// TestFreezeMonotonicClock_WakeDetected verifies that a jump in wall-clock time
// is correctly detected as a wake event, even if the monotonic clock was frozen.
func TestFreezeMonotonicClock_WakeDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.go")
	if err := os.WriteFile(path, []byte("wake"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a clock jump larger than the wake threshold on subsequent calls.
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
		// Advance the returned time past the threshold.
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

	// Seed the baseline with the initial clock reading.
	r.InjectWake()

	// Modify the file's modification time.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	// Trigger the second sweep which reads the advanced clock time.
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

// TestStart_WakeGapTriggersSweep verifies that running the background loop detects
// clock jumps and triggers a sweep reporting modified files.
func TestStart_WakeGapTriggersSweep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force the clock to jump ahead on subsequent tick reads.
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
	// Seed the baseline and modify the file on disk.
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

// TestStart_WakeGapWallClock verifies that the detector successfully detects a gap using
// wall-clock time even when the clock readings carry no monotonic component.
func TestStart_WakeGapWallClock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	base := time.Unix(1_700_000_000, 0) // no monotonic reading
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

	hits := make(chan string, 4)
	handler := func(_ context.Context, _, p string) { hits <- p }

	r := git.NewWakeReconciler(20*time.Millisecond, 500*time.Millisecond, handler, git.WithClock(nowFn))
	r.AddDir("repoA", dir)
	r.InjectWake() // seed mtime baseline (no nowFn call)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Start(ctx)

	select {
	case p := <-hits:
		if p != path {
			t.Fatalf("swept %q, want %q", p, path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no sweep on wall-clock gap with non-monotonic clock")
	}
}

// TestSeed_FirstWakeDetectsChange verifies that the initial sweep registers the file baseline
// without triggering events, while a subsequent sweep correctly reports post-seed modifications.
func TestSeed_FirstWakeDetectsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seeded.go")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
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
	r.InjectWake() // Seeding sweep registers the files but must not fire events.

	mu.Lock()
	if len(called) != 0 {
		t.Fatalf("seeding sweep must not fire the handler, got %v", called)
	}
	mu.Unlock()

	// Modify the file on disk after seeding.
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(called) != 1 || called[0] != path {
		t.Fatalf("first wake after Seed: got %v, want [%s]", called, path)
	}
}

// TestPrefixProbe_SameSizeSameMtime verifies that content changes within the prefix limit
// are detected even if the file size and modification time remain identical.
func TestPrefixProbe_SameSizeSameMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.go")
	const n = 100 // > prefixLen, fixed length so size never changes
	if err := os.WriteFile(path, []byte(strings.Repeat("A", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Store the original modification time to restore it later.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := info.ModTime()

	var mu sync.Mutex
	var called []string
	handler := func(_ context.Context, _, p string) {
		mu.Lock()
		called = append(called, p)
		mu.Unlock()
	}

	r := makeReconciler(handler, nil, time.Now)
	r.AddDir("repo1", dir)
	r.InjectWake() // Seed the baseline store.

	// Modify the content without changing the size, then restore the original modification time.
	if err := os.WriteFile(path, []byte(strings.Repeat("B", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, orig, orig); err != nil {
		t.Fatal(err)
	}
	r.InjectWake()

	mu.Lock()
	defer mu.Unlock()
	if len(called) != 1 || called[0] != path {
		t.Fatalf("prefix-probe change: got %v, want [%s]", called, path)
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
