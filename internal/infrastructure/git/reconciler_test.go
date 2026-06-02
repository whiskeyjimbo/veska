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

// TestIsRepoReconciling_PerRepo verifies the per-repo reconciling state: only
// the repo whose sweep is in flight reports true, a settled repo reports false,
// and the flag clears after the sweep completes. A blocking handler holds the
// target repo's sweep in flight while the test observes.
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
				close(ready) // repoA sweep is inside the handler
				<-proceed    // hold it in flight
			})
		}
	}

	// concurrency 1 so repoB does NOT sweep while repoA is held — that lets
	// us assert repoB reports false (settled) during repoA's in-flight sweep.
	r := git.NewWakeReconciler(100*time.Millisecond, 500*time.Millisecond, handler,
		git.WithClock(time.Now), git.WithWakeConcurrency(1))
	r.AddDir("repoA", dirA)
	r.AddDir("repoB", dirB)

	// Seed baseline (first inject — no handler calls expected).
	r.InjectWake()

	// Advance only repoA's file so its sweep fires the handler.
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

// TestSweepConcurrencyCap verifies that per-repo sweeps run in parallel but
// never exceed the configured wake_concurrency cap. A handler that blocks on a
// barrier lets the test count how many repo sweeps are simultaneously in
// flight. Run under -race to catch shared-state hazards.
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
		<-release // hold the sweep in flight until the test releases all
		mu.Lock()
		active--
		mu.Unlock()
	}

	r = git.NewWakeReconciler(100*time.Millisecond, 500*time.Millisecond, handler,
		git.WithClock(time.Now), git.WithWakeConcurrency(cap))
	for i, d := range dirs {
		r.AddDir(repoID(i), d)
	}

	// Seed baseline.
	r.InjectWake()

	// Advance every file so the next sweep fires the handler for each repo.
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
	// cap). They are all blocked on release, so this is a stable barrier.
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

// TestStart_WakeGapWallClock proves the detector fires on a wall-clock gap even
// when the clock readings carry NO monotonic component — the real post-suspend
// case, where CLOCK_MONOTONIC barely advanced but wall time jumped. time.Unix
// values have no monotonic reading, so a regression that reverted Start to a
// monotonic comparison would fail to see the gap here.
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

// TestSeed_FirstWakeDetectsChange verifies that an initial seeding sweep
// establishes a baseline so the SECOND wake sweep reports a file changed since
// the seed. The first sweep only records state (first-sighting) and fires
// nothing — the no-op first wake that solov2-w2r8's review flagged.
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
	r.InjectWake() // baseline (first-sighting) — no handler calls

	mu.Lock()
	if len(called) != 0 {
		t.Fatalf("seeding sweep must not fire the handler, got %v", called)
	}
	mu.Unlock()

	// Change the file AFTER seeding, then the FIRST wake must detect it.
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

// TestPrefixProbe_SameSizeSameMtime verifies the same-length-same-mtime hazard
// is caught: a file whose content changes in the first 64 bytes but whose size
// and mtime are identical (format-on-save behaviour) must still be reported.
func TestPrefixProbe_SameSizeSameMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fmt.go")
	const n = 100 // > prefixLen, fixed length so size never changes
	if err := os.WriteFile(path, []byte(strings.Repeat("A", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Capture the original mtime so we can restore it after the rewrite.
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
	r.InjectWake() // baseline (first-sighting)

	// Overwrite with the same length but different leading bytes, then force
	// the mtime back to the original so only the prefix distinguishes them.
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
