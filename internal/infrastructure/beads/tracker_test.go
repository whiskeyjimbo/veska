package beads_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/beads"
)

var _ ports.Tracker = (*beads.FileTracker)(nil)

func TestFileTracker_ActiveTask_NoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	tr := beads.NewFileTracker()
	task, err := tr.ActiveTask(context.Background(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
}

func TestFileTracker_ActiveTask_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "current_task"), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := beads.NewFileTracker()
	task, err := tr.ActiveTask(context.Background(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task for empty file, got %+v", task)
	}
}

func TestFileTracker_ActiveTask_WithID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	const wantID = "solov2-abc"
	if err := os.WriteFile(filepath.Join(dir, ".beads", "current_task"), []byte(wantID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := beads.NewFileTracker()
	task, err := tr.ActiveTask(context.Background(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task == nil {
		t.Fatal("expected non-nil task")
		return
	}
	if task.ID != wantID {
		t.Fatalf("ID: got %q, want %q", task.ID, wantID)
	}
	if task.RepoID != dir {
		t.Fatalf("RepoID: got %q, want %q", task.RepoID, dir)
	}
	if !task.Active {
		t.Fatal("expected task.Active == true")
	}
}

func TestFileTracker_RecentTasks_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	tr := beads.NewFileTracker()
	tasks, err := tr.RecentTasks(context.Background(), t.TempDir(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected empty slice, got %v", tasks)
	}
}
