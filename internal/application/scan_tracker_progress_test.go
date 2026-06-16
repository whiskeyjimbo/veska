package application

import "testing"

// verification — ScanTracker.Progress sets files_seen on the
// snapshot for an in-flight scan and is a no-op for unknown repos /
// nil receivers.

func TestScanTracker_ProgressUpdatesSnapshot(t *testing.T) {
	tr := NewScanTracker()
	tr.Start("repo-1")
	tr.Progress("repo-1", 250, 1000)
	snaps := tr.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("want 1 in-flight scan, got %d", len(snaps))
	}
	if snaps[0].FilesSeen != 250 || snaps[0].FilesTotal != 1000 {
		t.Errorf("want files_seen=250 files_total=1000, got %+v", snaps[0])
	}
}

func TestScanTracker_ProgressNoopForUnknownRepo(t *testing.T) {
	tr := NewScanTracker()
	tr.Progress("never-started", 42, 0)
	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("Progress on unknown repo must not create a scan entry; got %+v", got)
	}
}

func TestScanTracker_ProgressNilSafe(t *testing.T) {
	var tr *ScanTracker
	// Should not panic.
	tr.Progress("repo-1", 1, 2)
}

// TestScanTracker_ProgressEndClears verifies End clears the row even
// after Progress was called — otherwise the tracker pins the repo
// forever on long scans that fail mid-walk.
func TestScanTracker_ProgressEndClears(t *testing.T) {
	tr := NewScanTracker()
	tr.Start("repo-1")
	tr.Progress("repo-1", 100, 0)
	tr.End("repo-1")
	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("End should clear; got %+v", got)
	}
}
