package git

import (
	"context"
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
