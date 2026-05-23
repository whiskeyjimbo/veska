package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newSuppressionsDB(t *testing.T) *sql.DB {
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

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS suppressions (
			suppression_id TEXT PRIMARY KEY,
			scope          TEXT NOT NULL,
			target         TEXT NOT NULL,
			branch         TEXT,
			rule           TEXT,
			reason         TEXT NOT NULL,
			expires_at     INTEGER,
			created_at     INTEGER NOT NULL,
			actor_id       TEXT NOT NULL,
			actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system'))
		)
	`)
	if err != nil {
		t.Fatalf("create suppressions table: %v", err)
	}
	return db
}

func seedFindingForSuppression(t *testing.T, db *sql.DB, findingID, branch, repoID string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, NULL, 'main.go', 'low', 'security', 'test-rule', 'test message', 'open', ?, 'actor:seed', 'human')
	`, findingID, branch, repoID, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
}

func dispatchSuppression(t *testing.T, r *Registry, method string, actor domain.Actor, params map[string]any) (any, *RPCError) {
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

// ---------------------------------------------------------------------------
// eng_suppress_finding
// ---------------------------------------------------------------------------

// TestSuppressFinding_RejectsUnknownFinding covers solov2-b36: when scope is
// "finding" (the default) the handler must validate that (finding_id,
// branch, repo_id) actually exists in findings before inserting. Otherwise
// the suppressions table accumulates orphan rows that point at nothing
// and pollute eng_list_suppressions forever.
func TestSuppressFinding_RejectsUnknownFinding(t *testing.T) {
	db := newSuppressionsDB(t)
	// No finding seeded — only suppressions schema exists.

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "does-not-exist-xyz",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "should reject",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for unknown finding_id; got nil")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want CodeInvalidParams (%d)", rpcErr.Code, CodeInvalidParams)
	}
	// No suppression row should have been inserted on the rejected path.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions`).Scan(&n); err != nil {
		t.Fatalf("count suppressions: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 suppressions after rejected call, got %d", n)
	}
}

// TestSuppressFinding_AllowsNonFindingScopes covers the carve-out: when
// scope != "finding" (e.g. "rule" or "file") the target carries a
// different kind of identifier, so the finding-existence guard does not
// apply. The handler must let those calls through unchanged.
func TestSuppressFinding_AllowsNonFindingScopes(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "my-rule",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "wholesale silence the rule",
		"scope":      "rule",
	})
	if rpcErr != nil {
		t.Fatalf("scope=rule should not be validated against findings: %v", rpcErr.Message)
	}
}

func TestSuppressFinding_Basic(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-001", "main", "repo-1")

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-001",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "false positive",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	supID, _ := m["suppression_id"].(string)
	if supID == "" {
		t.Error("expected non-empty suppression_id in result")
	}

	// Verify row exists in DB.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions WHERE suppression_id = ?`, supID).Scan(&count); err != nil {
		t.Fatalf("query suppression: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 suppression row, got %d", count)
	}
}

func TestSuppressFinding_DefaultScope(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-002", "main", "repo-1")

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-002",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accepted risk",
		// no scope → defaults to "finding"
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	var scope string
	if err := db.QueryRow(`SELECT scope FROM suppressions WHERE target = 'finding-002'`).Scan(&scope); err != nil {
		t.Fatalf("query scope: %v", err)
	}
	if scope != "finding" {
		t.Errorf("expected scope='finding', got %q", scope)
	}
}

func TestSuppressFinding_WithExpiresAt(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-003", "main", "repo-1")

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	expiry := time.Now().Add(24 * time.Hour).UnixMilli()
	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-003",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "temporary waiver",
		"expires_at": expiry,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	var storedExpiry int64
	if err := db.QueryRow(`SELECT expires_at FROM suppressions WHERE target = 'finding-003'`).Scan(&storedExpiry); err != nil {
		t.Fatalf("query expires_at: %v", err)
	}
	if storedExpiry != expiry {
		t.Errorf("expected expires_at=%d, got %d", expiry, storedExpiry)
	}
}

func TestSuppressFinding_MissingParams(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"branch":  "main",
		"repo_id": "repo-1",
		"reason":  "some reason",
		// missing finding_id
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
}

// ---------------------------------------------------------------------------
// eng_list_suppressions
// ---------------------------------------------------------------------------

func TestListSuppressions_Empty(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchSuppression(t, r, "eng_list_suppressions", actor, map[string]any{
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
	items, _ := m["suppressions"].([]map[string]any)
	if len(items) != 0 {
		t.Errorf("expected 0 suppressions, got %d", len(items))
	}
}

func TestListSuppressions_AfterInsert(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-list-001", "main", "repo-1")

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	// Insert a suppression.
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-list-001",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accepted",
	})
	if rpcErr != nil {
		t.Fatalf("suppress: %v", rpcErr.Message)
	}

	// List suppressions.
	result, rpcErr := dispatchSuppression(t, r, "eng_list_suppressions", actor, map[string]any{
		"repo_id": "repo-1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("list: %v", rpcErr.Message)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	// We can't assert the exact type of the slice element since it's serialised via JSON.
	// Just ensure suppressions key exists and is non-empty.
	raw, err := json.Marshal(m["suppressions"])
	if err != nil {
		t.Fatalf("marshal suppressions: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal suppressions: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 suppression, got %d", len(rows))
	}
}
