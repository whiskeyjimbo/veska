// SPDX-License-Identifier: AGPL-3.0-only

package pollloop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunDrainsGreedily verifies that while step reports work, it is called
// back-to-back without waiting the idle interval - the whole point of the
// helper. A large interval would make this test hang if Run slept between
// calls instead of looping.
func TestRunDrainsGreedily(t *testing.T) {
	const work = 100
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		Run(ctx, time.Hour, func(context.Context) bool {
			// Report "did work" for the first `work` calls, then go idle.
			if calls.Add(1) <= work {
				return true
			}
			cancel() // first idle pass: end the loop
			return false
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not drain greedily within 5s (slept between work units?)")
	}
	if got := calls.Load(); got < work {
		t.Fatalf("step called %d times, want at least %d", got, work)
	}
}

// TestRunStopsOnContextCancel verifies Run returns promptly once ctx is
// canceled, even mid-idle.
func TestRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, time.Hour, func(context.Context) bool { return false }) // immediately idle
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancel")
	}
}
