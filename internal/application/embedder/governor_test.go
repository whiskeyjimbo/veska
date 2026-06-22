// SPDX-License-Identifier: AGPL-3.0-only

package embedder_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
)

func TestFixedGovernor_LimitClampedToOne(t *testing.T) {
	for _, in := range []int{0, -5} {
		if got := embedder.NewFixedGovernor(in).Limit(); got != 1 {
			t.Errorf("NewFixedGovernor(%d).Limit() = %d, want 1", in, got)
		}
	}
	if got := embedder.NewFixedGovernor(4).Limit(); got != 4 {
		t.Errorf("Limit() = %d, want 4", got)
	}
}

// TestFixedGovernor_BoundsConcurrency: a limit of 2 hands out two permits
// immediately; the third Acquire blocks until one is Released.
func TestFixedGovernor_BoundsConcurrency(t *testing.T) {
	g := embedder.NewFixedGovernor(2)
	ctx := context.Background()

	p1, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if _, err := g.Acquire(ctx); err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}

	// Third acquire must block while both slots are held.
	third := make(chan struct{})
	go func() {
		p3, _ := g.Acquire(ctx)
		_ = p3
		close(third)
	}()
	select {
	case <-third:
		t.Fatal("third Acquire returned while both slots held")
	case <-time.After(50 * time.Millisecond):
	}

	// Releasing one slot unblocks it.
	p1.Release(embedder.Outcome{})
	select {
	case <-third:
	case <-time.After(time.Second):
		t.Fatal("third Acquire did not unblock after Release")
	}
}

// TestFixedGovernor_RetryAfterPausesAcquire: an Outcome.RetryAfter on Release
// holds the next Acquire until the backoff elapses, even though slots are free.
func TestFixedGovernor_RetryAfterPausesAcquire(t *testing.T) {
	g := embedder.NewFixedGovernor(1)
	ctx := context.Background()

	p, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	const backoff = 120 * time.Millisecond
	p.Release(embedder.Outcome{RetryAfter: backoff})

	start := time.Now()
	p2, err := g.Acquire(ctx)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if waited := time.Since(start); waited < backoff*8/10 {
		t.Fatalf("Acquire returned after %s, want >= ~%s (Retry-After ignored)", waited, backoff)
	}
	p2.Release(embedder.Outcome{})
}

// TestFixedGovernor_AcquireRespectsCtxCancel: a canceled ctx unblocks an
// Acquire parked on a full semaphore.
func TestFixedGovernor_AcquireRespectsCtxCancel(t *testing.T) {
	g := embedder.NewFixedGovernor(1)
	if _, err := g.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	} // hold the only slot

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.Acquire(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return after ctx cancel")
	}
}

type retryAfterErr struct{ d time.Duration }

func (e retryAfterErr) Error() string             { return "rate limited" }
func (e retryAfterErr) RetryAfter() time.Duration { return e.d }

func TestRetryAfterFromErr(t *testing.T) {
	if got := embedder.RetryAfterFromErr(nil); got != 0 {
		t.Errorf("nil err: got %s, want 0", got)
	}
	if got := embedder.RetryAfterFromErr(errors.New("plain")); got != 0 {
		t.Errorf("plain err: got %s, want 0", got)
	}
	wrapped := errors.Join(errors.New("ctx"), retryAfterErr{d: 250 * time.Millisecond})
	if got := embedder.RetryAfterFromErr(wrapped); got != 250*time.Millisecond {
		t.Errorf("wrapped carrier: got %s, want 250ms", got)
	}
}
