package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RegisterTaskTools registers task management tools on r.
// db is the SQLite connection that backs the tasks table.
// aw is an optional AuditWriter; pass nil to disable audit logging.
func RegisterTaskTools(r *Registry, db *sql.DB, aw ports.AuditWriter) {
	r.MustRegister(ToolSpec{
		Name:            "eng_set_active_task",
		Description:     "Set the active task for a repo, deactivating any previously active task.",
		IncludesStaging: false,
		Handler:         makeSetActiveTaskHandler(db, aw),
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

func makeSetActiveTaskHandler(db *sql.DB, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p setActiveTaskParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("task_id", p.TaskID, "repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
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
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("task not found: %s in repo %s", p.TaskID, p.RepoID)}
		}

		if err := tx.Commit(); err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("commit tx: %v", err)}
		}

		if aw != nil {
			_ = aw.Write(ctx, ports.AuditEntry{
				RepoID:    p.RepoID,
				ActorID:   actor.ID,
				ActorKind: actor.Kind,
				Op:        "task.activate",
				TargetID:  p.TaskID,
				CreatedAt: time.Now(),
			})
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
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
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
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
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
