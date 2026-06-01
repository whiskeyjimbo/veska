package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
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

// TestTaskRepo_ActiveTask covers the domain.Task projection the MCP read uses:
// non-NULL tracker maps to non-nil pointers, created_at survives the round trip,
// and no active task returns (nil, nil).
func TestTaskRepo_ActiveTask(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	ctx := context.Background()
	repo := sqlite.NewTaskRepo(db)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`, "repo-1", "/tmp/repo-1", 1); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	got, err := repo.ActiveTask(ctx, "repo-1")
	if err != nil {
		t.Fatalf("ActiveTask (none): %v", err)
	}
	if got != nil {
		t.Fatalf("ActiveTask (none) = %+v, want nil", got)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO tasks (task_id, repo_id, tracker, tracker_ref, title, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"task-1", "repo-1", "jira", "ENG-7", "Fix the thing", 1, 12345); err != nil {
		t.Fatalf("seed active task: %v", err)
	}

	got, err = repo.ActiveTask(ctx, "repo-1")
	if err != nil {
		t.Fatalf("ActiveTask: %v", err)
	}
	if got == nil {
		t.Fatalf("ActiveTask = nil, want a task")
	}
	if got.ID != "task-1" || got.Title != "Fix the thing" || !got.Active {
		t.Fatalf("ActiveTask fields = %+v", got)
	}
	if got.Tracker == nil || *got.Tracker != "jira" || got.TrackerRef == nil || *got.TrackerRef != "ENG-7" {
		t.Fatalf("tracker pointers = %v/%v, want jira/ENG-7", got.Tracker, got.TrackerRef)
	}
	if got.CreatedAt.Unix() != 12345 {
		t.Fatalf("CreatedAt = %d, want 12345", got.CreatedAt.Unix())
	}
}

// TestTaskRepo_SetActiveTask covers the switch-active transaction: a match
// activates exactly one task (deactivating the incumbent) and reports found; a
// miss reports found=false and leaves the table untouched.
func TestTaskRepo_SetActiveTask(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	ctx := context.Background()
	repo := sqlite.NewTaskRepo(db)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`, "repo-1", "/tmp/repo-1", 1); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	for _, tk := range []struct {
		id     string
		active int
	}{{"task-A", 1}, {"task-B", 0}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO tasks (task_id, repo_id, title, active, created_at) VALUES (?, ?, ?, ?, ?)`,
			tk.id, "repo-1", tk.id, tk.active, 1); err != nil {
			t.Fatalf("seed %s: %v", tk.id, err)
		}
	}

	found, err := repo.SetActiveTask(ctx, "repo-1", "task-B")
	if err != nil {
		t.Fatalf("SetActiveTask: %v", err)
	}
	if !found {
		t.Fatalf("SetActiveTask found = false, want true")
	}
	assertActive(t, db, "task-A", 0)
	assertActive(t, db, "task-B", 1)

	// A miss leaves task-B active and reports not-found.
	found, err = repo.SetActiveTask(ctx, "repo-1", "task-missing")
	if err != nil {
		t.Fatalf("SetActiveTask (miss): %v", err)
	}
	if found {
		t.Fatalf("SetActiveTask (miss) found = true, want false")
	}
	assertActive(t, db, "task-B", 1)
}

// TestTaskRepo_ListTasks covers newest-first ordering and the limit cap.
func TestTaskRepo_ListTasks(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	ctx := context.Background()
	repo := sqlite.NewTaskRepo(db)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`, "repo-1", "/tmp/repo-1", 1); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	for i, created := range []int64{100, 300, 200} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO tasks (task_id, repo_id, title, active, created_at) VALUES (?, ?, ?, 0, ?)`,
			fmt.Sprintf("task-%d", i), "repo-1", "t", created); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
	}

	got, err := repo.ListTasks(ctx, "repo-1", 2)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListTasks len = %d, want 2 (limit)", len(got))
	}
	// Newest-first: created_at 300 then 200.
	if got[0].CreatedAt.Unix() != 300 || got[1].CreatedAt.Unix() != 200 {
		t.Fatalf("ordering = %d,%d, want 300,200", got[0].CreatedAt.Unix(), got[1].CreatedAt.Unix())
	}
}

func assertActive(t *testing.T, db *sql.DB, taskID string, want int) {
	t.Helper()
	var active int
	if err := db.QueryRow(`SELECT active FROM tasks WHERE task_id = ?`, taskID).Scan(&active); err != nil {
		t.Fatalf("query active %s: %v", taskID, err)
	}
	if active != want {
		t.Fatalf("%s active = %d, want %d", taskID, active, want)
	}
}
