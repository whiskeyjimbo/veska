package domain

import (
	"errors"
	"testing"
	"time"
)

// ── NewTask ────────────────────────────────────────────────────────────────

func TestNewTask_EmptyID(t *testing.T) {
	_, err := NewTask(TaskSpec{ID: "", RepoID: "repo1", Title: "My Task"})
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
		return
	}
}

func TestNewTask_EmptyRepoID(t *testing.T) {
	_, err := NewTask(TaskSpec{ID: "task-1", RepoID: "", Title: "My Task"})
	if err == nil {
		t.Fatal("expected error for empty repoID, got nil")
		return
	}
}

func TestNewTask_EmptyTitle(t *testing.T) {
	_, err := NewTask(TaskSpec{ID: "task-1", RepoID: "repo1", Title: ""})
	if err == nil {
		t.Fatal("expected error for empty title, got nil")
		return
	}
}

func TestNewTask_HappyPath(t *testing.T) {
	task, err := NewTask(TaskSpec{ID: "task-1", RepoID: "repo1", Title: "Do something"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.ID != "task-1" {
		t.Errorf("ID: got %q, want %q", task.ID, "task-1")
	}
	if task.RepoID != "repo1" {
		t.Errorf("RepoID: got %q, want %q", task.RepoID, "repo1")
	}
	if task.Title != "Do something" {
		t.Errorf("Title: got %q, want %q", task.Title, "Do something")
	}
	if task.Active {
		t.Error("Active should be false by default")
	}
	if task.Tracker != nil {
		t.Error("Tracker should be nil by default")
	}
	if task.TrackerRef != nil {
		t.Error("TrackerRef should be nil by default")
	}
	if task.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
}

func TestNewTask_WithTracker(t *testing.T) {
	task, err := NewTask(TaskSpec{ID: "task-1", RepoID: "repo1", Title: "Track me"}, WithTracker("bd", "bd:veska-42"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Tracker == nil || *task.Tracker != "bd" {
		t.Errorf("Tracker: got %v, want %q", task.Tracker, "bd")
	}
	if task.TrackerRef == nil || *task.TrackerRef != "bd:veska-42" {
		t.Errorf("TrackerRef: got %v, want %q", task.TrackerRef, "bd:veska-42")
	}
}

func TestNewTask_WithCreatedAt(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	task, err := NewTask(TaskSpec{ID: "task-1", RepoID: "repo1", Title: "Timed"}, WithCreatedAt(want))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !task.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt: got %v, want %v", task.CreatedAt, want)
	}
}

func TestNewTask_WithActive(t *testing.T) {
	task, err := NewTask(TaskSpec{ID: "task-1", RepoID: "repo1", Title: "Active task"}, WithActive())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !task.Active {
		t.Error("Active should be true after WithActive()")
	}
}

func TestNewTask_CreatedAtIsRecent(t *testing.T) {
	before := time.Now()
	task, err := NewTask(TaskSpec{ID: "task-1", RepoID: "repo1", Title: "Time check"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := time.Now()
	if task.CreatedAt.Before(before) || task.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v not in expected range [%v, %v]", task.CreatedAt, before, after)
	}
}

// ── TaskSet ────────────────────────────────────────────────────────────────

func TestTaskSet_AddInactiveTasks(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Task 1"})
	t2, _ := NewTask(TaskSpec{ID: "t2", RepoID: "repo1", Title: "Task 2"})
	if err := ts.Add(t1); err != nil {
		t.Fatalf("Add t1: %v", err)
	}
	if err := ts.Add(t2); err != nil {
		t.Fatalf("Add t2: %v", err)
	}
}

func TestTaskSet_AddNil(t *testing.T) {
	ts := NewTaskSet()
	if err := ts.Add(nil); err == nil {
		t.Fatal("expected error for nil task, got nil")
	}
}

func TestTaskSet_AddFirstActiveTaskSucceeds(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Task 1"}, WithActive())
	if err := ts.Add(t1); err != nil {
		t.Fatalf("Add first active task: %v", err)
	}
	if got := ts.Active("repo1"); got == nil || got.ID != "t1" {
		t.Errorf("Active: got %v, want t1", got)
	}
}

func TestTaskSet_AddSecondActiveTaskReturnsDuplicateError(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Task 1"}, WithActive())
	t2, _ := NewTask(TaskSpec{ID: "t2", RepoID: "repo1", Title: "Task 2"}, WithActive())
	_ = ts.Add(t1)
	err := ts.Add(t2)
	if !errors.Is(err, ErrDuplicateActiveTask) {
		t.Errorf("expected ErrDuplicateActiveTask, got %v", err)
	}
}

func TestTaskSet_AddActiveTasksDifferentReposOK(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Task 1"}, WithActive())
	t2, _ := NewTask(TaskSpec{ID: "t2", RepoID: "repo2", Title: "Task 2"}, WithActive())
	if err := ts.Add(t1); err != nil {
		t.Fatalf("Add t1: %v", err)
	}
	if err := ts.Add(t2); err != nil {
		t.Fatalf("Add t2 (different repo): %v", err)
	}
}

func TestTaskSet_ActiveReturnsNilForUnknownRepo(t *testing.T) {
	ts := NewTaskSet()
	if got := ts.Active("unknown-repo"); got != nil {
		t.Errorf("Active: got %v, want nil", got)
	}
}

func TestTaskSet_ActiveReturnsNilWhenNoActiveTask(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Inactive"})
	_ = ts.Add(t1)
	if got := ts.Active("repo1"); got != nil {
		t.Errorf("Active: got %v, want nil", got)
	}
}

func TestTaskSet_SetActiveDeactivatesPrevious(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Task 1"}, WithActive())
	t2, _ := NewTask(TaskSpec{ID: "t2", RepoID: "repo1", Title: "Task 2"})
	_ = ts.Add(t1)
	_ = ts.Add(t2)

	ts.SetActive(t2)

	active := ts.Active("repo1")
	if active == nil || active.ID != "t2" {
		t.Errorf("Active after SetActive: got %v, want t2", active)
	}
	if t1.Active {
		t.Error("t1 should have been deactivated by SetActive")
	}
}

func TestTaskSet_SetActiveTaskNotInSet(t *testing.T) {
	ts := NewTaskSet()
	t1, _ := NewTask(TaskSpec{ID: "t1", RepoID: "repo1", Title: "Orphan"})
	// t1 was not previously added to the set; SetActive should still set it as the active task.
	ts.SetActive(t1)
	if got := ts.Active("repo1"); got == nil || got.ID != "t1" {
		t.Errorf("Active: got %v, want t1", got)
	}
}
