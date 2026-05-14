package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RegisterTaskTools registers task management tools on r.
// db is the SQLite connection that backs the tasks table.
func RegisterTaskTools(r *Registry, db *sql.DB) {
	r.MustRegister(ToolSpec{
		Name:            "eng_set_active_task",
		Description:     "Set the active task for a repo, deactivating any previously active task.",
		IncludesStaging: false,
		Handler:         makeSetActiveTaskHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_active_task",
		Description:     "Get the currently active task for a repo, or null if none is active.",
		IncludesStaging: false,
		Handler:         makeGetActiveTaskHandler(db),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_task_history",
		Description:     "Get task history for a repo ordered by creation time, newest first.",
		IncludesStaging: false,
		Handler:         makeGetTaskHistoryHandler(db),
	})
}

// ---------------------------------------------------------------------------
// eng_set_active_task
// ---------------------------------------------------------------------------

type setActiveTaskParams struct {
	TaskID string `json:"task_id"`
	RepoID string `json:"repo_id"`
}

func makeSetActiveTaskHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p setActiveTaskParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.TaskID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "task_id is required"}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}

		// Deactivate all tasks for this repo, then activate the target task.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("begin tx: %v", err)}
		}
		defer tx.Rollback() //nolint:errcheck

		if _, err = tx.ExecContext(ctx,
			`UPDATE tasks SET active = 0 WHERE repo_id = ?`, p.RepoID,
		); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("deactivate tasks: %v", err)}
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET active = 1 WHERE task_id = ? AND repo_id = ?`, p.TaskID, p.RepoID,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("activate task: %v", err)}
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("task not found: %s in repo %s", p.TaskID, p.RepoID)}
		}

		if err := tx.Commit(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("commit tx: %v", err)}
		}

		return map[string]any{
			"task_id": p.TaskID,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// eng_get_active_task
// ---------------------------------------------------------------------------

type getActiveTaskParams struct {
	RepoID string `json:"repo_id"`
}

type taskRow struct {
	TaskID     string  `json:"task_id"`
	RepoID     string  `json:"repo_id"`
	Tracker    *string `json:"tracker,omitempty"`
	TrackerRef *string `json:"tracker_ref,omitempty"`
	Title      string  `json:"title"`
	Active     int     `json:"active"`
	CreatedAt  int64   `json:"created_at"`
}

func makeGetActiveTaskHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getActiveTaskParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}

		var t taskRow
		err := db.QueryRowContext(ctx,
			`SELECT task_id, repo_id, tracker, tracker_ref, title, active, created_at
			   FROM tasks WHERE repo_id = ? AND active = 1`,
			p.RepoID,
		).Scan(&t.TaskID, &t.RepoID, &t.Tracker, &t.TrackerRef, &t.Title, &t.Active, &t.CreatedAt)
		if err == sql.ErrNoRows {
			return map[string]any{"task_id": nil}, nil
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query active task: %v", err)}
		}

		return t, nil
	}
}

// ---------------------------------------------------------------------------
// eng_get_task_history
// ---------------------------------------------------------------------------

type getTaskHistoryParams struct {
	RepoID string `json:"repo_id"`
	Limit  int    `json:"limit,omitempty"`
}

const defaultTaskHistoryLimit = 20

func makeGetTaskHistoryHandler(db *sql.DB) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getTaskHistoryParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.RepoID == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
		}
		limit := p.Limit
		if limit <= 0 {
			limit = defaultTaskHistoryLimit
		}

		rows, err := db.QueryContext(ctx,
			`SELECT task_id, repo_id, tracker, tracker_ref, title, active, created_at
			   FROM tasks WHERE repo_id = ? ORDER BY created_at DESC LIMIT ?`,
			p.RepoID, limit,
		)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query tasks: %v", err)}
		}
		defer rows.Close()

		tasks := make([]taskRow, 0)
		for rows.Next() {
			var t taskRow
			if err := rows.Scan(
				&t.TaskID, &t.RepoID, &t.Tracker, &t.TrackerRef,
				&t.Title, &t.Active, &t.CreatedAt,
			); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("scan task: %v", err)}
			}
			tasks = append(tasks, t)
		}
		if err := rows.Err(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("iterate tasks: %v", err)}
		}

		return map[string]any{
			"tasks": tasks,
		}, nil
	}
}
