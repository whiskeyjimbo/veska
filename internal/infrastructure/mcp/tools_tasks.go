package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TaskStore is the narrow port the task tools need from the tasks-table adapter.
// *sqlite.TaskRepo satisfies it structurally; keeping the interface beside its
// sole consumer (the rule of thumb in CLAUDE.md) is why the tasks-table SQL no
// longer leaks into this inbound-adapter layer.
type TaskStore interface {
	// ActiveTask returns repoID's active task, or (nil, nil) when none is active.
	ActiveTask(ctx context.Context, repoID string) (*domain.Task, error)
	// SetActiveTask makes taskID the sole active task for repoID. found is false
	// (nil error) when no task matches (taskID, repoID).
	SetActiveTask(ctx context.Context, repoID, taskID string) (found bool, err error)
	// ListTasks returns repoID's tasks newest-first, capped at limit rows.
	ListTasks(ctx context.Context, repoID string, limit int) ([]domain.Task, error)
}

// RegisterTaskTools registers task management tools on r.
// store backs the tasks table; aw is an optional AuditWriter (pass nil to
// disable audit logging).
//
// PARKED — not wired into the daemon. The composition root keeps only a
// keep-alive reference to this function (see internal/cli/daemon/mcptools.go)
// and never calls it, because there is no MCP path to *create* a task yet, so
// exposing eng_set_active_task / eng_get_active_task / eng_get_task_history
// would surface a dead-end (callers get -32601 method not found). The specs
// and handlers below are kept compiling + unit-tested so the feature
// re-registers cleanly once a task backend lands; they are exercised only by
// the coverage harness's WithTaskTools() option, never the live registry.
func RegisterTaskTools(r *Registry, store TaskStore, aw ports.AuditWriter) {
	r.MustRegister(ToolSpec{
		Name:            "eng_set_active_task",
		Description:     "Set the active task for a repo, deactivating any previously active task.",
		IncludesStaging: false,
		Handler:         makeSetActiveTaskHandler(store, aw),
		InputSchema:     setActiveTaskInputSchema,
		OutputSchema:    setActiveTaskOutputSchema,
		CLIExempt:       ExemptAgentOnly,
		ExemptReason:    "active-task is a per-MCP-session anchor for context-pack auto-seeding; the CLI's one-shot model doesn't carry the session state this targets.",
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_active_task",
		Description:     "Get the currently active task for a repo, or null if none is active.",
		IncludesStaging: false,
		Handler:         makeGetActiveTaskHandler(store),
		CLIExempt:       ExemptAgentOnly,
		ExemptReason:    "paired with eng_set_active_task; same session-state argument.",
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_task_history",
		Description:     "Get task history for a repo ordered by creation time, newest first.",
		IncludesStaging: false,
		Handler:         makeGetTaskHistoryHandler(store),
		CLIExempt:       ExemptAgentOnly,
		ExemptReason:    "task tooling is end-to-end an MCP-session surface; no CLI consumer.",
	})
}

// ---------------------------------------------------------------------------
// eng_set_active_task
// ---------------------------------------------------------------------------

type setActiveTaskParams struct {
	TaskID string `json:"task_id"`
	RepoID string `json:"repo_id"`
}

// setActiveTaskInputSchema describes the params object for eng_set_active_task.
var setActiveTaskInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": {"type": "string", "description": "ID of the task to activate."},
    "repo_id": {"type": "string", "description": "Repository the task belongs to."}
  },
  "required": ["task_id", "repo_id"]
}`)

// setActiveTaskOutputSchema describes the result object for eng_set_active_task.
var setActiveTaskOutputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "task_id": {"type": "string"}
  },
  "required": ["task_id"]
}`)

func makeSetActiveTaskHandler(store TaskStore, aw ports.AuditWriter) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p setActiveTaskParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("task_id", p.TaskID, "repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
		}

		found, err := store.SetActiveTask(ctx, p.RepoID, p.TaskID)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("set active task: %v", err)}
		}
		if !found {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("task not found: %s in repo %s", p.TaskID, p.RepoID)}
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

// newTaskRow maps a domain.Task to the tools' wire shape: the integer active
// column (0/1), the unix-seconds created_at, and nil tracker pointers omitted
// from the JSON — the projection these MCP tools have always emitted.
func newTaskRow(t domain.Task) taskRow {
	active := 0
	if t.Active {
		active = 1
	}
	return taskRow{
		TaskID:     t.ID,
		RepoID:     t.RepoID,
		Tracker:    t.Tracker,
		TrackerRef: t.TrackerRef,
		Title:      t.Title,
		Active:     active,
		CreatedAt:  t.CreatedAt.Unix(),
	}
}

func makeGetActiveTaskHandler(store TaskStore) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getActiveTaskParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID); rpcErr != nil {
			return nil, rpcErr
		}

		t, err := store.ActiveTask(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query active task: %v", err)}
		}
		if t == nil {
			return map[string]any{"task_id": nil}, nil
		}

		return newTaskRow(*t), nil
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

func makeGetTaskHistoryHandler(store TaskStore) ToolHandler {
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

		got, err := store.ListTasks(ctx, p.RepoID, limit)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query tasks: %v", err)}
		}

		tasks := make([]taskRow, 0, len(got))
		for _, t := range got {
			tasks = append(tasks, newTaskRow(t))
		}

		return map[string]any{
			"tasks": tasks,
		}, nil
	}
}
