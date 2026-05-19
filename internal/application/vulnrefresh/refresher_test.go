package vulnrefresh_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/vulnrefresh"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeSource is a controllable ports.VulnSource. Each Refresh call signals on
// the calls channel so a test can observe invocations without sleeping; the
// error returned is configurable.
type fakeSource struct {
	calls chan struct{}

	mu  sync.Mutex
	err error
	n   int64
}

func newFakeSource() *fakeSource {
	return &fakeSource{calls: make(chan struct{}, 64)}
}

func (f *fakeSource) Refresh(ctx context.Context) error {
	atomic.AddInt64(&f.n, 1)
	f.mu.Lock()
	err := f.err
	f.mu.Unlock()
	select {
	case f.calls <- struct{}{}:
	default:
	}
	return err
}

func (f *fakeSource) Scan(ctx context.Context, deps []ports.Dependency) ([]ports.VulnFinding, error) {
	return nil, nil
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeSource) count() int64 { return atomic.LoadInt64(&f.n) }

// waitForCalls blocks until the source has been invoked at least n times or
// the deadline expires.
func waitForCalls(t *testing.T, f *fakeSource, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := range n {
		select {
		case <-f.calls:
		case <-deadline:
			t.Fatalf("timed out waiting for call %d/%d (got %d)", i+1, n, f.count())
		}
	}
}

func TestNewRefresher_NilSource(t *testing.T) {
	if _, err := vulnrefresh.NewRefresher(nil); !errors.Is(err, vulnrefresh.ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency, got %v", err)
	}
}

func TestDefaultInterval(t *testing.T) {
	if vulnrefresh.DefaultInterval != 24*time.Hour {
		t.Fatalf("DefaultInterval = %v, want 24h", vulnrefresh.DefaultInterval)
	}
	r, err := vulnrefresh.NewRefresher(newFakeSource())
	if err != nil {
		t.Fatalf("NewRefresher: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil Refresher")
		return
	}
}

func TestRun_RefreshesOnStartAndEachTick(t *testing.T) {
	f := newFakeSource()
	r, err := vulnrefresh.NewRefresher(f, vulnrefresh.WithInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRefresher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Immediate call on entry, then repeated calls on the tick.
	waitForCalls(t, f, 4)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRun_ErrorIsIsolated(t *testing.T) {
	f := newFakeSource()
	f.setErr(errors.New("boom"))
	r, err := vulnrefresh.NewRefresher(f, vulnrefresh.WithInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRefresher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Errors must not stop the ticker: calls keep coming despite Refresh failing.
	waitForCalls(t, f, 4)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel; error halted the loop")
	}
}

func TestRun_StopsPromptlyOnCancel(t *testing.T) {
	f := newFakeSource()
	// A long interval: the loop must exit via ctx, not by waiting a tick.
	r, err := vulnrefresh.NewRefresher(f, vulnrefresh.WithInterval(time.Hour))
	if err != nil {
		t.Fatalf("NewRefresher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// The immediate refresh fires regardless of interval.
	waitForCalls(t, f, 1)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly on cancel")
	}

	if got := f.count(); got != 1 {
		t.Fatalf("expected exactly 1 refresh before cancel, got %d", got)
	}
}

func TestWithInterval_IgnoresNonPositive(t *testing.T) {
	f := newFakeSource()
	r, err := vulnrefresh.NewRefresher(f, vulnrefresh.WithInterval(0), vulnrefresh.WithInterval(-1))
	if err != nil {
		t.Fatalf("NewRefresher: %v", err)
	}
	if r.Interval() != vulnrefresh.DefaultInterval {
		t.Fatalf("Interval = %v, want default %v", r.Interval(), vulnrefresh.DefaultInterval)
	}
}
