// TaskRepo is the SQLite adapter for the tasks table. It keeps tasks-table SQL
// out of its two callers — the contextpack assembler's active-task lookup
// (wire.go binds GetActiveTask to contextpack.ActiveTaskFunc) and the MCP task
// tools (ActiveTask/SetActiveTask/ListTasks) — so the SQL and row mapping live
// here, beside the other sqlite.*Repo adapters.

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TaskRepo reads and writes the tasks table over a pool.
type TaskRepo struct {
	db *sql.DB
}

// NewTaskRepo constructs a TaskRepo backed by db.
func NewTaskRepo(db *sql.DB) *TaskRepo {
	return &TaskRepo{db: db}
}

// activeTask is the single SELECT both active-task projections share. It returns
// the repo's active task as a domain.Task (or nil when none is active), keeping
// tracker/tracker_ref as nil pointers on NULL so each caller can map the absence
// as it needs.
func (r *TaskRepo) activeTask(ctx context.Context, repoID string) (*domain.Task, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT task_id, repo_id, tracker, tracker_ref, title, active, created_at
		   FROM tasks WHERE repo_id = ? AND active = 1`,
		repoID,
	)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("active task lookup: %w", err)
	}
	return t, nil
}

// GetActiveTask returns the repo's active task as a contextpack.TaskInfo, or
// (nil, nil) when none is active. NULL tracker/tracker_ref map to empty strings
// — the projection the contextpack assembler expects.
func (r *TaskRepo) GetActiveTask(ctx context.Context, repoID string) (*contextpack.TaskInfo, error) {
	t, err := r.activeTask(ctx, repoID)
	if err != nil || t == nil {
		return nil, err
	}
	return &contextpack.TaskInfo{
		TaskID:     t.ID,
		RepoID:     t.RepoID,
		Tracker:    derefOr(t.Tracker, ""),
		TrackerRef: derefOr(t.TrackerRef, ""),
		Title:      t.Title,
		Active:     t.Active,
	}, nil
}

// ActiveTask returns the repo's active task as a domain.Task, or (nil, nil) when
// none is active. It is the MCP task tools' read; NULL tracker fields stay nil.
func (r *TaskRepo) ActiveTask(ctx context.Context, repoID string) (*domain.Task, error) {
	return r.activeTask(ctx, repoID)
}

// SetActiveTask makes taskID the sole active task for repoID, deactivating any
// previously active task in one transaction. found is false (with a nil error)
// when no task matches (taskID, repoID), letting the caller report not-found.
func (r *TaskRepo) SetActiveTask(ctx context.Context, repoID, taskID string) (found bool, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.ExecContext(ctx,
		`UPDATE tasks SET active = 0 WHERE repo_id = ?`, repoID,
	); err != nil {
		return false, fmt.Errorf("deactivate tasks: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET active = 1 WHERE task_id = ? AND repo_id = ?`, taskID, repoID,
	)
	if err != nil {
		return false, fmt.Errorf("activate task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit tx: %w", err)
	}
	return true, nil
}

// ListTasks returns repoID's tasks newest-first, capped at limit rows.
func (r *TaskRepo) ListTasks(ctx context.Context, repoID string, limit int) ([]domain.Task, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT task_id, repo_id, tracker, tracker_ref, title, active, created_at
		   FROM tasks WHERE repo_id = ? ORDER BY created_at DESC LIMIT ?`,
		repoID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]domain.Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

// scanner is the read surface shared by *sql.Row and *sql.Rows so scanTask can
// serve both the single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

// scanTask maps one tasks row to a domain.Task. NULL tracker/tracker_ref become
// nil pointers; the integer active column maps to a bool; created_at (unix
// seconds) maps to a time.Time.
func scanTask(s scanner) (*domain.Task, error) {
	var (
		t                   domain.Task
		tracker, trackerRef sql.NullString
		active              int
		createdAt           int64
	)
	if err := s.Scan(&t.ID, &t.RepoID, &tracker, &trackerRef, &t.Title, &active, &createdAt); err != nil {
		return nil, err
	}
	if tracker.Valid {
		t.Tracker = &tracker.String
	}
	if trackerRef.Valid {
		t.TrackerRef = &trackerRef.String
	}
	t.Active = active != 0
	t.CreatedAt = time.Unix(createdAt, 0)
	return &t, nil
}

// derefOr returns *p, or def when p is nil.
func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
