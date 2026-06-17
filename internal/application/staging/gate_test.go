// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package staging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestWaitIfPaused_NotPaused verifies that WaitIfPaused returns immediately
// when the gate is not paused.
func TestWaitIfPaused_NotPaused(t *testing.T) {
	sa := NewArea()
	g := NewGate(sa)

	done := make(chan struct{})
	go func() {
		g.WaitIfPaused()
		close(done)
	}()

	select {
	case <-done:
		// expected: returned immediately
	case <-time.After(100 * time.Millisecond):
		t.Fatal("WaitIfPaused should return immediately when gate is not paused")
	}
}

// TestWaitIfPaused_BlocksAndUnblocks verifies that WaitIfPaused blocks while
// the gate is paused and unblocks after Resume is called.
func TestWaitIfPaused_BlocksAndUnblocks(t *testing.T) {
	sa := NewArea()
	g := NewGate(sa)

	g.Pause()

	unblocked := make(chan struct{})
	go func() {
		g.WaitIfPaused()
		close(unblocked)
	}()

	// Goroutine must still be blocked after a short wait.
	select {
	case <-unblocked:
		t.Fatal("WaitIfPaused should block while gate is paused")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	g.Resume()

	select {
	case <-unblocked:
		// expected: unblocked after Resume
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitIfPaused did not unblock after Resume")
	}
}

// TestBumpGeneration_Increments verifies that BumpGeneration returns
// monotonically increasing values and Generation reflects the current value.
func TestBumpGeneration_Increments(t *testing.T) {
	sa := NewArea()
	g := NewGate(sa)

	if gen := g.Generation(); gen != 0 {
		t.Fatalf("expected initial generation 0, got %d", gen)
	}

	v1 := g.BumpGeneration()
	if v1 != 1 {
		t.Fatalf("expected BumpGeneration to return 1, got %d", v1)
	}
	if g.Generation() != 1 {
		t.Fatalf("expected Generation() == 1, got %d", g.Generation())
	}

	v2 := g.BumpGeneration()
	if v2 != 2 {
		t.Fatalf("expected BumpGeneration to return 2, got %d", v2)
	}
	if v2 <= v1 {
		t.Fatalf("BumpGeneration must be strictly increasing: v1=%d v2=%d", v1, v2)
	}
}

// TestBranchSwitch_ClearsAndResumes verifies that BranchSwitch clears staging for
// the previous branch, calls drainFn, and leaves the gate unpaused.
func TestBranchSwitch_ClearsAndResumes(t *testing.T) {
	sa := NewArea()

	// Stage some data on prevBranch.
	n, err := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	sa.Stage("repo1", "main", "a.go", File{Nodes: []*domain.Node{n}, Edges: nil})

	// Verify it is there.
	if files := sa.StagedFiles("repo1", "main"); len(files) != 1 {
		t.Fatalf("pre-condition: expected 1 staged file, got %d", len(files))
	}

	g := NewGate(sa)

	drainCalled := false
	drainFn := func(_ context.Context) error {
		drainCalled = true
		return nil
	}

	if err := g.BranchSwitch(context.Background(), "repo1", "main", drainFn); err != nil {
		t.Fatalf("BranchSwitch returned unexpected error: %v", err)
	}

	if !drainCalled {
		t.Fatal("drainFn was not called by BranchSwitch")
	}

	// Staging for prevBranch must be cleared.
	if files := sa.StagedFiles("repo1", "main"); len(files) != 0 {
		t.Fatalf("expected staging to be cleared after BranchSwitch, got %d files", len(files))
	}

	// Gate must be unpaused.
	if g.IsPaused() {
		t.Fatal("gate must not be paused after BranchSwitch completes")
	}
}

// TestBranchSwitch_DrainError_StillResumes verifies that a drainFn error does
// not leave the gate paused (no deadlock).
func TestBranchSwitch_DrainError_StillResumes(t *testing.T) {
	sa := NewArea()
	g := NewGate(sa)

	drainErr := errors.New("queue drain failed")
	drainFn := func(_ context.Context) error {
		return drainErr
	}

	err := g.BranchSwitch(context.Background(), "repo1", "main", drainFn)
	if !errors.Is(err, drainErr) {
		t.Fatalf("expected drainErr to be propagated, got: %v", err)
	}

	// Gate must be unpaused even after drainFn error.
	if g.IsPaused() {
		t.Fatal("gate must not remain paused after drainFn error (deadlock risk)")
	}

	// WaitIfPaused must not block.
	done := make(chan struct{})
	go func() {
		g.WaitIfPaused()
		close(done)
	}()
	select {
	case <-done:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("WaitIfPaused blocked after drainFn error - gate stuck paused")
	}
}

// TestStaleGenerationWrite verifies that StageIfCurrentGeneration discards writes
// whose generation counter is stale and accepts writes at the current generation.
func TestStaleGenerationWrite(t *testing.T) {
	sa := NewArea()
	g := NewGate(sa)

	n, err := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	// Capture generation 0, then bump so 0 is now stale.
	staleGen := g.Generation() // 0
	g.BumpGeneration()         // current is now 1

	// Stale write must be rejected.
	ok := sa.Stage("repo1", "main", "a.go", File{Nodes: []*domain.Node{n}, Edges: nil}, WithGenerationGuard(staleGen, g))
	if ok {
		t.Fatal("StageIfCurrentGeneration must return false for stale generation")
	}
	if files := sa.StagedFiles("repo1", "main"); len(files) != 0 {
		t.Fatalf("stale write must not appear in staging, got %d files", len(files))
	}

	// Current-generation write must be accepted.
	currentGen := g.Generation() // 1
	ok = sa.Stage("repo1", "main", "a.go", File{Nodes: []*domain.Node{n}, Edges: nil}, WithGenerationGuard(currentGen, g))
	if !ok {
		t.Fatal("StageIfCurrentGeneration must return true for current generation")
	}
	if files := sa.StagedFiles("repo1", "main"); len(files) != 1 {
		t.Fatalf("current-gen write must appear in staging, got %d files", len(files))
	}
}

// TestConcurrentWaitIfPaused verifies that multiple goroutines blocked in
// WaitIfPaused are all released when Resume is called.
func TestConcurrentWaitIfPaused(t *testing.T) {
	sa := NewArea()
	g := NewGate(sa)

	const workers = 10
	g.Pause()

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			g.WaitIfPaused()
			wg.Done()
		}()
	}

	// Give goroutines time to block.
	time.Sleep(20 * time.Millisecond)

	g.Resume()

	// All goroutines must unblock within a reasonable time.
	released := make(chan struct{})
	go func() {
		wg.Wait()
		close(released)
	}()

	select {
	case <-released:
		// expected: all goroutines released
	case <-time.After(500 * time.Millisecond):
		t.Fatal("not all WaitIfPaused goroutines were released after Resume")
	}
}
