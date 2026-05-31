// TaskRepo is the SQLite read adapter for the tasks table's active-task lookup.
// It keeps the contextpack assembler's active-task query out of the daemon
// composition root: the SQL and row mapping live here, beside the other
// sqlite.*Repo adapters, while wire.go binds the method value to
// contextpack.ActiveTaskFunc.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
)

// TaskRepo reads the tasks table over a read pool.
type TaskRepo struct {
	db *sql.DB
}

// NewTaskRepo constructs a TaskRepo backed by db (the read pool).
func NewTaskRepo(db *sql.DB) *TaskRepo {
	return &TaskRepo{db: db}
}

// GetActiveTask returns the repo's active task, or (nil, nil) when none is
// active. tracker/tracker_ref are NULL-able and map to empty strings; the
// integer active column maps to a bool (any nonzero value is true).
func (r *TaskRepo) GetActiveTask(ctx context.Context, repoID string) (*contextpack.TaskInfo, error) {
	var (
		t                   contextpack.TaskInfo
		tracker, trackerRef sql.NullString
		active              int
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT task_id, repo_id, tracker, tracker_ref, title, active
		   FROM tasks WHERE repo_id = ? AND active = 1`,
		repoID,
	).Scan(&t.TaskID, &t.RepoID, &tracker, &trackerRef, &t.Title, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("active task lookup: %w", err)
	}
	t.Tracker = tracker.String
	t.TrackerRef = trackerRef.String
	t.Active = active != 0
	return &t, nil
}
