// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

func newTasksDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqldriver.Name, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS repos (
			repo_id  TEXT PRIMARY KEY,
			root_path TEXT NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create repos table: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS tasks (
			task_id     TEXT PRIMARY KEY,
			repo_id     TEXT NOT NULL,
			tracker     TEXT,
			tracker_ref TEXT,
			title       TEXT NOT NULL,
			active      INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL,
			FOREIGN KEY (repo_id) REFERENCES repos(repo_id)
		)
	`)
	if err != nil {
		t.Fatalf("create tasks table: %v", err)
	}

	// A partial unique index is used to enforce at most one active task per repository.
	_, err = db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_one_per_repo ON tasks(repo_id) WHERE active = 1
	`)
	if err != nil {
		t.Fatalf("create tasks index: %v", err)
	}

	return db
}

func seedRepo(t *testing.T, db *sql.DB, repoID, rootPath string) {
	t.Helper()
	_, err := db.Exec(`INSERT OR IGNORE INTO repos (repo_id, root_path) VALUES (?, ?)`, repoID, rootPath)
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
}

func seedTask(t *testing.T, db *sql.DB, taskID, repoID, title string, active int) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO tasks (task_id, repo_id, title, active, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, taskID, repoID, title, active, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func dispatchTask(t *testing.T, r *Registry, method string, actor domain.Actor, params map[string]any) (any, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	}
	return r.Dispatch(context.Background(), actor, req)
}

func TestSetActiveTask_Basic(t *testing.T) {
	db := newTasksDB(t)
	seedRepo(t, db, "repo-1", "/repos/repo-1")
	seedTask(t, db, "task-001", "repo-1", "First task", 0)

	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	result, rpcErr := dispatchTask(t, r, "eng_set_active_task", actor, map[string]any{
		"task_id": "task-001",
		"repo_id": "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if m["task_id"] != "task-001" {
		t.Errorf("expected task_id=task-001, got %v", m["task_id"])
	}

	var active int
	if err := db.QueryRow(`SELECT active FROM tasks WHERE task_id = 'task-001'`).Scan(&active); err != nil {
		t.Fatalf("query active: %v", err)
	}
	if active != 1 {
		t.Errorf("expected active=1, got %d", active)
	}
}

func TestSetActiveTask_SwitchesActiveTask(t *testing.T) {
	db := newTasksDB(t)
	seedRepo(t, db, "repo-1", "/repos/repo-1")
	seedTask(t, db, "task-A", "repo-1", "Task A", 1)
	seedTask(t, db, "task-B", "repo-1", "Task B", 0)

	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchTask(t, r, "eng_set_active_task", actor, map[string]any{
		"task_id": "task-B",
		"repo_id": "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	var activeA, activeB int
	_ = db.QueryRow(`SELECT active FROM tasks WHERE task_id = 'task-A'`).Scan(&activeA)
	_ = db.QueryRow(`SELECT active FROM tasks WHERE task_id = 'task-B'`).Scan(&activeB)

	if activeA != 0 {
		t.Errorf("expected task-A active=0, got %d", activeA)
	}
	if activeB != 1 {
		t.Errorf("expected task-B active=1, got %d", activeB)
	}
}

func TestSetActiveTask_MissingParams(t *testing.T) {
	db := newTasksDB(t)
	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchTask(t, r, "eng_set_active_task", actor, map[string]any{
		"repo_id": "repo-1",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

func TestGetActiveTask_NoActive(t *testing.T) {
	db := newTasksDB(t)
	seedRepo(t, db, "repo-1", "/repos/repo-1")

	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchTask(t, r, "eng_get_active_task", actor, map[string]any{
		"repo_id": "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if m["task_id"] != nil {
		t.Errorf("expected task_id=nil, got %v", m["task_id"])
	}
}

func TestGetActiveTask_WithActive(t *testing.T) {
	db := newTasksDB(t)
	seedRepo(t, db, "repo-1", "/repos/repo-1")
	seedTask(t, db, "task-active", "repo-1", "Active task", 1)

	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchTask(t, r, "eng_get_active_task", actor, map[string]any{
		"repo_id": "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if m["task_id"] != "task-active" {
		t.Errorf("expected task_id=task-active, got %v", m["task_id"])
	}
}

func TestGetTaskHistory_DefaultLimit(t *testing.T) {
	db := newTasksDB(t)
	seedRepo(t, db, "repo-1", "/repos/repo-1")

	// TestGetTaskHistory_DefaultLimit seeds more than 20 tasks to ensure that the default history response is capped at 20.
	for i := range 25 {
		taskID := "task-" + string(rune('A'+i))
		_, err := db.Exec(`
			INSERT INTO tasks (task_id, repo_id, title, active, created_at)
			VALUES (?, ?, ?, 0, ?)
		`, taskID, "repo-1", "Task "+taskID, time.Now().UnixNano()+int64(i))
		if err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
	}

	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchTask(t, r, "eng_get_task_history", actor, map[string]any{
		"repo_id": "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tasksRaw, err := json.Marshal(m["tasks"])
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	var tasks []map[string]any
	if err := json.Unmarshal(tasksRaw, &tasks); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}
	if len(tasks) != 20 {
		t.Errorf("expected 20 tasks (default limit), got %d", len(tasks))
	}
}

func TestGetTaskHistory_CustomLimit(t *testing.T) {
	db := newTasksDB(t)
	seedRepo(t, db, "repo-1", "/repos/repo-1")
	seedTask(t, db, "task-X", "repo-1", "Task X", 0)
	seedTask(t, db, "task-Y", "repo-1", "Task Y", 0)
	seedTask(t, db, "task-Z", "repo-1", "Task Z", 0)

	r := NewRegistry()
	RegisterTaskTools(r, sqlite.NewTaskRepo(db), nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchTask(t, r, "eng_get_task_history", actor, map[string]any{
		"repo_id": "repo-1",
		"limit":   2,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tasksRaw, err := json.Marshal(m["tasks"])
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	var tasks []map[string]any
	if err := json.Unmarshal(tasksRaw, &tasks); err != nil {
		t.Fatalf("unmarshal tasks: %v", err)
	}

	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks with limit=2, got %d", len(tasks))
	}
}
