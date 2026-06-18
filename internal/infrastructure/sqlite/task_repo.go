// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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

// TaskRepo is the SQLite adapter for the tasks table.
type TaskRepo struct {
	db *sql.DB
}

// NewTaskRepo constructs a TaskRepo backed by db.
func NewTaskRepo(db *sql.DB) *TaskRepo {
	return &TaskRepo{db: db}
}

// activeTask retrieves the active task for a repository, keeping tracker and trackerRef
// fields nil if they are NULL in the database.
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

// GetActiveTask returns the repo's active task, mapping NULL tracker fields to
// empty strings as required by the contextpack assembler.
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

// ActiveTask returns the active task as a domain.Task, leaving NULL tracker fields
// as nil pointers.
func (r *TaskRepo) ActiveTask(ctx context.Context, repoID string) (*domain.Task, error) {
	return r.activeTask(ctx, repoID)
}

// SetActiveTask deactivates any existing active task and activates the specified task
// in a single transaction. Returns false if the target task does not exist.
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

// scanner abstracts sql.Row and sql.Rows to allow unified scanning of task rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanTask scans a single task row and maps database types to domain.Task fields.
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

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
