// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package application_test

import (
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
)

// TestScanTracker_StartEndSnapshotLifecycle covers the basic surface:
// Start adds a repo, Snapshot reports it, End removes it, repeat is safe.
func TestScanTracker_StartEndSnapshotLifecycle(t *testing.T) {
	tr := application.NewScanTracker()

	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("fresh tracker should be empty; got %+v", got)
	}

	tr.Start("repo-a")
	tr.Start("repo-b")

	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	// Stable, sorted-by-RepoID order so the JSON shape doesn't churn.
	if snap[0].RepoID != "repo-a" || snap[1].RepoID != "repo-b" {
		t.Errorf("Snapshot order = %v, want [repo-a, repo-b]",
			[]string{snap[0].RepoID, snap[1].RepoID})
	}
	for _, s := range snap {
		if s.Phase != "running" {
			t.Errorf("phase for %q = %q, want 'running'", s.RepoID, s.Phase)
		}
		if s.StartedAt.IsZero() {
			t.Errorf("StartedAt for %q is zero", s.RepoID)
		}
	}

	tr.End("repo-a")
	snap = tr.Snapshot()
	if len(snap) != 1 || snap[0].RepoID != "repo-b" {
		t.Errorf("after End(repo-a), Snapshot = %+v", snap)
	}

	// End on an unknown id is a no-op.
	tr.End("never-was-here")
	if got := tr.Snapshot(); len(got) != 1 {
		t.Errorf("End on unknown id changed state: %+v", got)
	}
}

// TestScanTracker_NilSafe pins that Start/End/Snapshot tolerate a nil
// receiver - that's the contract callers (statusProvider, the reparser)
// rely on for "no tracker wired" graceful degradation.
func TestScanTracker_NilSafe(t *testing.T) {
	var tr *application.ScanTracker
	tr.Start("x")
	tr.End("x")
	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("nil tracker Snapshot = %+v, want empty", got)
	}
}

// TestScanTracker_ConcurrentStartEnd: hammer the tracker from many
// goroutines to exercise the RWMutex. -race must stay clean.
func TestScanTracker_ConcurrentStartEnd(t *testing.T) {
	tr := application.NewScanTracker()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func(i int) { defer wg.Done(); tr.Start(repoKey(i)) }(i)
		go func(i int) { defer wg.Done(); tr.End(repoKey(i)) }(i)
	}
	wg.Wait()
	// Snapshot must work without panic; we don't assert content because the
	// Start/End ordering is racy by design.
	_ = tr.Snapshot()
}

func repoKey(i int) string { return "r" + string(rune('0'+i%10)) }
