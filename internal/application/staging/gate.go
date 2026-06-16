package staging

import (
	"context"
	"sync"
	"sync/atomic"
)

// Gate controls the branch-switch quiescence protocol.
// It guards an Area with a monotonic generation counter so
// in-flight saves from the prior branch cannot corrupt the new branch's staging.
// Pause/Resume use a sync.Cond so blocked goroutines sleep without spinning.
// BumpGeneration uses atomic increment for lock-free reads by the hot save path.
// BranchSwitch defers Resume to guarantee the gate is always unpaused, even on
// drainFn panic.
type Gate struct {
	staging *Area

	// gen is the current staging generation. Atomically incremented on each
	// branch switch. Hot-path reads (Stage's generation guard) compare against
	// this without holding a lock.
	gen atomic.Uint64

	// mu protects paused. cond is signaled by Resume.
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
}

// NewGate constructs a Gate wired to the given Area.
// The gate starts unpaused at generation 0.
func NewGate(staging *Area) *Gate {
	g := &Gate{staging: staging}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Generation returns the current staging generation counter.
// The value is read atomically; no lock is held.
func (g *Gate) Generation() uint64 {
	return g.gen.Load()
}

// BumpGeneration atomically increments the generation counter and returns the
// new value. Called at the start of every branch switch so that in-flight saves
// carrying the old generation are silently discarded.
func (g *Gate) BumpGeneration() uint64 {
	return g.gen.Add(1)
}

// IsPaused returns true while a branch-switch quiescence is in progress.
func (g *Gate) IsPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

// Pause marks the gate as paused. Subsequent callers to WaitIfPaused will block
// until Resume is called. If the gate is already paused this is a no-op.
func (g *Gate) Pause() {
	g.mu.Lock()
	g.paused = true
	g.mu.Unlock()
}

// Resume unpauses the gate and broadcasts to all goroutines blocked in
// WaitIfPaused. If the gate is already unpaused this is a no-op.
func (g *Gate) Resume() {
	g.mu.Lock()
	g.paused = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

// WaitIfPaused blocks until the gate is unpaused. Returns immediately if the
// gate is not paused. Used by the Ingester.Save hot path to hold writes during
// a branch switch without spinning.
func (g *Gate) WaitIfPaused() {
	g.mu.Lock()
	for g.paused {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// BranchSwitch executes the full quiescence sequence:
//  1. BumpGeneration — invalidates all in-flight saves from the prior branch.
//  2. Pause — new Save calls will block at WaitIfPaused.
//  3. staging.Clear(repoID, prevBranch) — drops stale overlay data.
//  4. drainFn(ctx) — caller drains the post_promotion_queue; blocks until empty
//     or ctx is done.
//  5. Resume — releases any goroutines blocked in WaitIfPaused.
//
// Resume is deferred so it always fires even if drainFn panics, preventing a
// permanent deadlock. The error from drainFn (if non-nil) is returned to the
// caller after the gate has been resumed.
func (g *Gate) BranchSwitch(ctx context.Context, repoID, prevBranch string, drainFn func(ctx context.Context) error) error {
	g.BumpGeneration()
	g.Pause()
	defer g.Resume()

	g.staging.Clear(repoID, prevBranch)

	return drainFn(ctx)
}
