package ports

import (
	"context"
	"time"
)

// TaskSummary is the ports-layer view of a unit of tracked work. It carries
// only the fields needed by application-layer callers; the full domain.Task may
// hold richer data that is not surfaced here.
type TaskSummary struct {
	// ID is the stable identifier assigned by the tracker (e.g. a beads task ID).
	ID string

	// RepoID is the repository this task is scoped to.
	RepoID string

	// Title is a short human-readable description of the work.
	Title string

	// Active is true when this task is the currently active task for its repo.
	Active bool

	// CreatedAt is the wall-clock time at which the task was recorded.
	CreatedAt time.Time
}

// Tracker is the port for querying active and recent tasks from an external
// issue tracker. Implementations are provided by infrastructure adapters
// (e.g. beads file-based tracker, Jira, Linear).
type Tracker interface {
	// ActiveTask returns the currently active task for repoID, or nil if no
	// task is active. A nil result is not an error.
	ActiveTask(ctx context.Context, repoID string) (*TaskSummary, error)

	// RecentTasks returns the most recent tasks for repoID, newest first.
	// At most limit tasks are returned. An empty slice is not an error.
	RecentTasks(ctx context.Context, repoID string, limit int) ([]TaskSummary, error)
}
