// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"
	"time"
)

// TaskSummary carries only the fields needed by application-layer callers; the
// full domain.Task may hold richer data not surfaced here.
type TaskSummary struct {
	// ID is the stable identifier assigned by the tracker.
	ID string

	RepoID string

	Title string

	// Active is true when this task is the currently active task for its repository.
	Active bool

	CreatedAt time.Time
}

// Tracker is the port for querying active and recent tasks from an external
// issue tracker.
type Tracker interface {
	// ActiveTask returns the currently active task, or nil if no task is active.
	// A nil result is not an error.
	ActiveTask(ctx context.Context, repoID string) (*TaskSummary, error)

	RecentTasks(ctx context.Context, repoID string, limit int) ([]TaskSummary, error)
}
