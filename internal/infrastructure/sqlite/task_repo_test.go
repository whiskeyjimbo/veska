// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestTaskRepo_GetActiveTask verifies that active tasks map to populated TaskInfo,
// returns nil when no task is active, and maps NULL tracker fields to empty strings.
func TestTaskRepo_GetActiveTask(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	ctx := context.Background()
	repo := sqlite.NewTaskRepo(db)

	// Insert a repository to satisfy the foreign key constraint.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`, "repo-1", "/tmp/repo-1", 1); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	got, err := repo.GetActiveTask(ctx, "repo-1")
	if err != nil {
		t.Fatalf("GetActiveTask (none): %v", err)
	}
	if got != nil {
		t.Fatalf("GetActiveTask (none) = %+v, want nil", got)
	}

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

// TestTaskRepo_ActiveTask verifies that ActiveTask correctly maps SQL fields to domain.Task pointers and values.
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

// TestTaskRepo_SetActiveTask verifies that SetActiveTask atomically updates the active task and deactivates any existing active task.
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

	found, err = repo.SetActiveTask(ctx, "repo-1", "task-missing")
	if err != nil {
		t.Fatalf("SetActiveTask (miss): %v", err)
	}
	if found {
		t.Fatalf("SetActiveTask (miss) found = true, want false")
	}
	assertActive(t, db, "task-B", 1)
}

// TestTaskRepo_ListTasks verifies that ListTasks returns tasks sorted by creation time in descending order and honors the limit parameter.
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
