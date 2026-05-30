package domain

import (
	"errors"
	"time"
)

// ErrDuplicateActiveTask is returned by TaskSet.Add when a second active task
// is inserted for the same repo.
var ErrDuplicateActiveTask = errors.New("task: a task is already active for this repo")

// Task represents a unit of work scoped to a single repository.
//
// The one-active-task-per-repo invariant cannot be enforced by Task alone;
// use TaskSet to manage collections of tasks within a repo scope.
type Task struct {
	ID         string
	RepoID     string
	Tracker    *string
	TrackerRef *string
	Title      string
	Active     bool
	CreatedAt  time.Time
}

// TaskOption is a functional option for NewTask.
type TaskOption func(*Task) error

// WithTracker sets the optional tracker name and ref on the task.
func WithTracker(tracker, ref string) TaskOption {
	return func(t *Task) error {
		t.Tracker = &tracker
		t.TrackerRef = &ref
		return nil
	}
}

// WithActive marks the task as the active task for its repo.
// Use TaskSet.SetActive to switch the active task in a collection.
func WithActive() TaskOption {
	return func(t *Task) error {
		t.Active = true
		return nil
	}
}

// WithCreatedAt overrides the default creation timestamp (time.Now()). It makes
// NewTask deterministic for tests — mirroring the injected createdAt that
// NewSuppression takes — without burdening the common call site with a
// timestamp argument.
func WithCreatedAt(t time.Time) TaskOption {
	return func(task *Task) error {
		task.CreatedAt = t
		return nil
	}
}

// NewTask constructs a validated Task. Returns an error if id, repoID, or
// title is empty. CreatedAt defaults to time.Now(); pass WithCreatedAt to set
// it deterministically.
func NewTask(id, repoID, title string, opts ...TaskOption) (*Task, error) {
	if id == "" {
		return nil, errors.New("task: id must not be empty")
	}
	if repoID == "" {
		return nil, errors.New("task: repo_id must not be empty")
	}
	if title == "" {
		return nil, errors.New("task: title must not be empty")
	}

	t := &Task{
		ID:        id,
		RepoID:    repoID,
		Title:     title,
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
// invariant.
//
// TaskSet is NOT safe for concurrent use without external synchronisation.
type TaskSet struct {
	// tasks holds all tasks indexed by ID.
	tasks map[string]*Task
	// activeByRepo holds the currently active task per repoID.
	activeByRepo map[string]*Task
}

// NewTaskSet returns an empty TaskSet.
func NewTaskSet() *TaskSet {
	return &TaskSet{
		tasks:        make(map[string]*Task),
		activeByRepo: make(map[string]*Task),
	}
}

// Add inserts t into the set. Returns ErrDuplicateActiveTask if t.Active is
// true and a different task is already marked active for t.RepoID.
func (ts *TaskSet) Add(t *Task) error {
	if t.Active {
		if existing, ok := ts.activeByRepo[t.RepoID]; ok && existing.ID != t.ID {
			return ErrDuplicateActiveTask
		}
		ts.activeByRepo[t.RepoID] = t
	}
	ts.tasks[t.ID] = t
	return nil
}

// Active returns the currently active task for repoID, or nil if none exists.
func (ts *TaskSet) Active(repoID string) *Task {
	return ts.activeByRepo[repoID]
}

// SetActive activates t and deactivates any previously active task for t.RepoID.
// If t is not already in the set it is added.
//
// Unlike Add, SetActive cannot violate the one-active-per-repo invariant — it
// deactivates the incumbent before promoting t — so it returns no error.
func (ts *TaskSet) SetActive(t *Task) {
	// Deactivate the previous active task for this repo, if any.
	if prev, ok := ts.activeByRepo[t.RepoID]; ok && prev.ID != t.ID {
		prev.Active = false
	}

	t.Active = true
	ts.activeByRepo[t.RepoID] = t
	ts.tasks[t.ID] = t
}
