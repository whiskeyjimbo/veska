package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestTaskRepo_GetActiveTask covers the three behaviours the old wire.go closure
// had: an active task maps to a fully-populated TaskInfo with Active=true; no
// active task returns (nil, nil); and NULL tracker/tracker_ref map to "".
func TestTaskRepo_GetActiveTask(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	ctx := context.Background()
	repo := sqlite.NewTaskRepo(db)

	// repos row to satisfy the tasks.repo_id foreign key.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`, "repo-1", "/tmp/repo-1", 1); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// No active task yet -> (nil, nil).
	got, err := repo.GetActiveTask(ctx, "repo-1")
	if err != nil {
		t.Fatalf("GetActiveTask (none): %v", err)
	}
	if got != nil {
		t.Fatalf("GetActiveTask (none) = %+v, want nil", got)
	}

	// Insert an active task with non-NULL tracker fields.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (task_id, repo_id, tracker, tracker_ref, title, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"task-1", "repo-1", "jira", "ENG-7", "Fix the thing", 1, 100); err != nil {
		t.Fatalf("seed active task: %v", err)
	}

	got, err = repo.GetActiveTask(ctx, "repo-1")
	if err != nil {
		t.Fatalf("GetActiveTask: %v", err)
	}
	if got == nil {
		t.Fatalf("GetActiveTask = nil, want a task")
	}
	if got.TaskID != "task-1" || got.RepoID != "repo-1" || got.Title != "Fix the thing" {
		t.Fatalf("GetActiveTask fields = %+v", got)
	}
	if got.Tracker != "jira" || got.TrackerRef != "ENG-7" {
		t.Fatalf("tracker fields = %q/%q, want jira/ENG-7", got.Tracker, got.TrackerRef)
	}
	if !got.Active {
		t.Fatalf("Active = false, want true")
	}

	// A task with NULL tracker/tracker_ref maps to empty strings.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`, "repo-2", "/tmp/repo-2", 1); err != nil {
		t.Fatalf("seed repo-2: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (task_id, repo_id, title, active, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"task-2", "repo-2", "Null tracker task", 1, 200); err != nil {
		t.Fatalf("seed null-tracker task: %v", err)
	}

	got, err = repo.GetActiveTask(ctx, "repo-2")
	if err != nil {
		t.Fatalf("GetActiveTask (null tracker): %v", err)
	}
	if got == nil {
		t.Fatalf("GetActiveTask (null tracker) = nil, want a task")
	}
	if got.Tracker != "" || got.TrackerRef != "" {
		t.Fatalf("NULL tracker fields = %q/%q, want empty", got.Tracker, got.TrackerRef)
	}
}
