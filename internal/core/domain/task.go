// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

import (
	"errors"
	"time"
)

// ErrDuplicateActiveTask is returned when attempting to add a second active task
// for the same repository.
var ErrDuplicateActiveTask = errors.New("task: a task is already active for this repo")

// Task represents a unit of work scoped to a single repository. To enforce the
// invariant of at most one active task per repository, use TaskSet to manage
// task collections.
type Task struct {
	ID         string
	RepoID     string
	Tracker    *string
	TrackerRef *string
	Title      string
	Active     bool
	CreatedAt  time.Time
}

type TaskOption func(*Task) error

// WithTracker sets the optional tracker name and reference on the task.
func WithTracker(tracker, ref string) TaskOption {
	return func(t *Task) error {
		t.Tracker = &tracker
		t.TrackerRef = &ref
		return nil
	}
}

// WithActive marks the task as active.
func WithActive() TaskOption {
	return func(t *Task) error {
		t.Active = true
		return nil
	}
}

// WithCreatedAt overrides the default creation timestamp, primarily for testing determinism.
func WithCreatedAt(t time.Time) TaskOption {
	return func(task *Task) error {
		task.CreatedAt = t
		return nil
	}
}

// TaskSpec groups the required fields of a Task into a struct to prevent
// transposing adjacent same-typed parameters at construction call sites.
type TaskSpec struct {
	ID     string
	RepoID string
	Title  string
}

// NewTask constructs a validated Task from the specification, verifying that
// spec.ID, spec.RepoID, and spec.Title are non-empty.
func NewTask(spec TaskSpec, opts ...TaskOption) (*Task, error) {
	if spec.ID == "" {
		return nil, errors.New("task: id must not be empty")
	}
	if spec.RepoID == "" {
		return nil, errors.New("task: repo_id must not be empty")
	}
	if spec.Title == "" {
		return nil, errors.New("task: title must not be empty")
	}

	t := &Task{
		ID:        spec.ID,
		RepoID:    spec.RepoID,
		Title:     spec.Title,
		CreatedAt: time.Now(),
	}

	for _, opt := range opts {
		if err := opt(t); err != nil {
			return nil, err
		}
	}

	return t, nil
}

// TaskSet manages a collection of Tasks and enforces the one-active-task-per-repo
// invariant. It is not safe for concurrent use without external synchronization.
type TaskSet struct {
	tasks        map[string]*Task
	activeByRepo map[string]*Task
}

func NewTaskSet() *TaskSet {
	return &TaskSet{
		tasks:        make(map[string]*Task),
		activeByRepo: make(map[string]*Task),
	}
}

// Add inserts a task into the set, returning ErrDuplicateActiveTask if the task
// is active and another task is already active for the repository.
func (ts *TaskSet) Add(t *Task) error {
	if t == nil {
		return errors.New("task: task must not be nil")
	}
	if t.Active {
		if existing, ok := ts.activeByRepo[t.RepoID]; ok && existing.ID != t.ID {
			return ErrDuplicateActiveTask
		}
		ts.activeByRepo[t.RepoID] = t
	}
	ts.tasks[t.ID] = t
	return nil
}

// Active returns the currently active task for the repository, or nil if none is active.
func (ts *TaskSet) Active(repoID string) *Task {
	return ts.activeByRepo[repoID]
}

// SetActive activates the task, deactivating the previously active task for the
// repository, and adds the task to the set if not already present.
func (ts *TaskSet) SetActive(t *Task) {
	if prev, ok := ts.activeByRepo[t.RepoID]; ok && prev.ID != t.ID {
		prev.Active = false
	}

	t.Active = true
	ts.activeByRepo[t.RepoID] = t
	ts.tasks[t.ID] = t
}
