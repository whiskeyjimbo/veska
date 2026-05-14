package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newFindingsDB creates an in-memory SQLite DB seeded with the findings table.
func newFindingsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS findings (
			finding_id    TEXT NOT NULL,
			branch        TEXT NOT NULL,
			repo_id       TEXT NOT NULL,
			node_id       TEXT,
			file_path     TEXT,
			severity      TEXT NOT NULL,
			source_layer  TEXT NOT NULL,
			rule          TEXT NOT NULL,
			message       TEXT NOT NULL,
			state         TEXT NOT NULL,
			closed_reason TEXT,
			created_at    INTEGER NOT NULL,
			closed_at     INTEGER,
			actor_id      TEXT NOT NULL,
			actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
			PRIMARY KEY (finding_id, branch)
		)
	`)
	if err != nil {
		t.Fatalf("create findings table: %v", err)
	}
	return db
}

// seedFinding inserts a finding row for use in tests.
func seedFinding(t *testing.T, db *sql.DB, findingID, branch, repoID, severity, state string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, NULL, 'main.go', ?, 'security', 'test-rule', 'test message', ?, ?, 'actor:seed', 'human')
	`, findingID, branch, repoID, severity, state, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
}

// dispatchFinding dispatches eng_close_finding with the given actor and params.
func dispatchFinding(t *testing.T, r *Registry, actor domain.Actor, params map[string]string) (any, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{
		JSONRPC: "2.0",
		Method:  "eng_close_finding",
		Params:  raw,
	}
	return r.Dispatch(context.Background(), actor, req)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCloseFindings_HumanActionGate(t *testing.T) {
	tests := []struct {
		name       string
		severity   string
		actor      domain.Actor
		wantCode   int    // 0 means expect success
		wantReason string // substring in Message for error cases
	}{
		{
			name:       "high + agent → refuse",
			severity:   "high",
			actor:      domain.Actor{ID: "agent:claude", Kind: domain.ActorKindAgent},
			wantCode:   CodeHumanRequired,
			wantReason: "human_required",
		},
		{
			name:       "critical + agent → refuse",
			severity:   "critical",
			actor:      domain.Actor{ID: "agent:claude", Kind: domain.ActorKindAgent},
			wantCode:   CodeHumanRequired,
			wantReason: "human_required",
		},
		{
			name:     "high + human → accept",
			severity: "high",
			actor:    domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman},
			wantCode: 0,
		},
		{
			name:     "low + agent → accept",
			severity: "low",
			actor:    domain.Actor{ID: "agent:claude", Kind: domain.ActorKindAgent},
			wantCode: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newFindingsDB(t)
			findingID := "finding-" + tc.severity + "-" + string(tc.actor.Kind)
			seedFinding(t, db, findingID, "main", "repo-1", tc.severity, "open")

			r := NewRegistry()
			RegisterFindingTools(r, db)

			result, rpcErr := dispatchFinding(t, r, tc.actor, map[string]string{
				"finding_id": findingID,
				"branch":     "main",
				"repo_id":    "repo-1",
				"reason":     "resolved",
			})

			if tc.wantCode != 0 {
				// Expect an RPC error.
				if rpcErr == nil {
					t.Fatalf("expected RPC error with code %d, got result: %v", tc.wantCode, result)
				}
				if rpcErr.Code != tc.wantCode {
					t.Errorf("expected code %d, got %d", tc.wantCode, rpcErr.Code)
				}
				if tc.wantReason != "" && !strings.Contains(rpcErr.Message, tc.wantReason) {
					t.Errorf("expected message to contain %q, got %q", tc.wantReason, rpcErr.Message)
				}
				// Verify finding was NOT closed in DB.
				var state string
				err := db.QueryRow(`SELECT state FROM findings WHERE finding_id = ? AND branch = ?`, findingID, "main").Scan(&state)
				if err != nil {
					t.Fatalf("query state: %v", err)
				}
				if state != "open" {
					t.Errorf("expected state=open after refusal, got %q", state)
				}
				return
			}

			// Expect success.
			if rpcErr != nil {
				t.Fatalf("unexpected RPC error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
			}

			// Verify finding was closed in DB.
			var state string
			err := db.QueryRow(`SELECT state FROM findings WHERE finding_id = ? AND branch = ?`, findingID, "main").Scan(&state)
			if err != nil {
				t.Fatalf("query state: %v", err)
			}
			if state != "closed" {
				t.Errorf("expected state=closed, got %q", state)
			}
		})
	}
}

func TestCloseFindings_MessageContainsFindingAndSeverity(t *testing.T) {
	db := newFindingsDB(t)
	const fid = "finding-critical-001"
	seedFinding(t, db, fid, "main", "repo-1", "critical", "open")

	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": fid,
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "resolved",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error")
	}
	if !strings.Contains(rpcErr.Message, fid) {
		t.Errorf("expected finding_id %q in message, got: %q", fid, rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "critical") {
		t.Errorf("expected severity in message, got: %q", rpcErr.Message)
	}
}

func TestCloseFindings_NotFound(t *testing.T) {
	db := newFindingsDB(t)

	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "no-such-finding",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "resolved",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error for not-found finding")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
}

func TestCloseFindings_MissingParams(t *testing.T) {
	db := newFindingsDB(t)

	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	// Missing finding_id.
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"branch":  "main",
		"repo_id": "repo-1",
		"reason":  "resolved",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
}

// ---------------------------------------------------------------------------
// eng_list_findings tests
// ---------------------------------------------------------------------------

func dispatchListFindings(t *testing.T, r *Registry, actor domain.Actor, params map[string]string) (any, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{
		JSONRPC: "2.0",
		Method:  "eng_list_findings",
		Params:  raw,
	}
	return r.Dispatch(context.Background(), actor, req)
}

func TestListFindings_Empty(t *testing.T) {
	db := newFindingsDB(t)
	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	items, _ := m["findings"].([]findingRow)
	if len(items) != 0 {
		t.Errorf("expected 0 findings, got %d", len(items))
	}
}

func TestListFindings_DefaultStateIsOpen(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "open-f", "main", "repo-1", "low", "open")
	seedFinding(t, db, "closed-f", "main", "repo-1", "low", "closed")

	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1",
		"branch":  "main",
		// no state → defaults to "open"
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	m := result.(map[string]any)
	findings, _ := m["findings"].([]findingRow)
	if len(findings) != 1 {
		t.Errorf("expected 1 open finding, got %d", len(findings))
	}
	if len(findings) == 1 && findings[0].FindingID != "open-f" {
		t.Errorf("expected finding_id=open-f, got %q", findings[0].FindingID)
	}
}

func TestListFindings_SeverityFilter(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "low-f", "main", "repo-1", "low", "open")
	seedFinding(t, db, "high-f", "main", "repo-1", "high", "open")

	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id":  "repo-1",
		"branch":   "main",
		"severity": "high",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	m := result.(map[string]any)
	findings, _ := m["findings"].([]findingRow)
	if len(findings) != 1 {
		t.Errorf("expected 1 high finding, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// eng_reopen_finding tests
// ---------------------------------------------------------------------------

func dispatchReopenFinding(t *testing.T, r *Registry, actor domain.Actor, params map[string]string) (any, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{
		JSONRPC: "2.0",
		Method:  "eng_reopen_finding",
		Params:  raw,
	}
	return r.Dispatch(context.Background(), actor, req)
}

func TestReopenFinding_Basic(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "reopen-f", "main", "repo-1", "low", "closed")

	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"finding_id": "reopen-f",
		"branch":     "main",
		"repo_id":    "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if m["state"] != "open" {
		t.Errorf("expected state=open, got %v", m["state"])
	}

	// Verify DB.
	var state string
	if err := db.QueryRow(`SELECT state FROM findings WHERE finding_id = 'reopen-f' AND branch = 'main'`).Scan(&state); err != nil {
		t.Fatalf("query state: %v", err)
	}
	if state != "open" {
		t.Errorf("expected state=open in DB, got %q", state)
	}
}

func TestReopenFinding_AnyActorCanReopen(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "reopen-agent", "main", "repo-1", "critical", "closed")

	r := NewRegistry()
	RegisterFindingTools(r, db)

	// An agent should be able to reopen even a critical finding (no human gate).
	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"finding_id": "reopen-agent",
		"branch":     "main",
		"repo_id":    "repo-1",
	})
	if rpcErr != nil {
		t.Fatalf("agent should be able to reopen any finding, got error: %v", rpcErr.Message)
	}
}

func TestReopenFinding_NotFound(t *testing.T) {
	db := newFindingsDB(t)
	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"finding_id": "no-such",
		"branch":     "main",
		"repo_id":    "repo-1",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for not-found finding")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

func TestReopenFinding_MissingParams(t *testing.T) {
	db := newFindingsDB(t)
	r := NewRegistry()
	RegisterFindingTools(r, db)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"branch":  "main",
		"repo_id": "repo-1",
		// missing finding_id
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}
