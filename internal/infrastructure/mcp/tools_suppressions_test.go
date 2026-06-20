// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

func newSuppressionsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqldriver.Name, ":memory:")
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

// When the scope is 'finding', the handler validates that the target finding exists in the database to prevent orphaned suppressions.
func TestSuppressFinding_RejectsUnknownFinding(t *testing.T) {
	db := newSuppressionsDB(t)

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil, nil)

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
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions`).Scan(&n); err != nil {
		t.Fatalf("count suppressions: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 suppressions after rejected call, got %d", n)
	}
}

// Non-finding scopes like 'rule' or 'file' bypass target existence checks because they target virtual categories rather than specific database rows.
func TestSuppressFinding_AllowsNonFindingScopes(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil, nil)

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
	RegisterSuppressionTools(r, db, nil, nil)

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
	RegisterSuppressionTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-002",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accepted risk",
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
	RegisterSuppressionTools(r, db, nil, nil)

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

// repo_id resolution for scope='finding' must accept a short prefix, the full
// id, or omission - matching eng_close_finding / eng_get_finding. A genuinely
// different repo_id must still be rejected.
func TestSuppressFinding_RepoIDResolutionParity(t *testing.T) {
	const fullRepoID = "1436fd395322aabbccddeeff00112233445566778899aabbccddeeff00112233"
	const shortRepoID = "1436fd395322"

	cases := []struct {
		name      string
		findingID string
		repoID    string
		wantErr   bool
	}{
		{name: "short prefix", findingID: "finding-short", repoID: shortRepoID, wantErr: false},
		{name: "full id", findingID: "finding-full", repoID: fullRepoID, wantErr: false},
		{name: "omitted", findingID: "finding-omit", repoID: "", wantErr: false},
		{name: "wrong repo", findingID: "finding-wrong", repoID: "deadbeef", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newSuppressionsDB(t)
			seedFindingForSuppression(t, db, tc.findingID, "main", fullRepoID)

			r := NewRegistry()
			RegisterSuppressionTools(r, db, nil, nil)

			actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
			params := map[string]any{
				"finding_id": tc.findingID,
				"branch":     "main",
				"reason":     "parity check",
			}
			if tc.repoID != "" {
				params["repo_id"] = tc.repoID
			}
			_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, params)

			if tc.wantErr {
				if rpcErr == nil {
					t.Fatal("expected RPC error for mismatched repo_id; got nil")
				}
				if rpcErr.Code != CodeInvalidParams {
					t.Errorf("error code = %d, want CodeInvalidParams (%d)", rpcErr.Code, CodeInvalidParams)
				}
				return
			}
			if rpcErr != nil {
				t.Fatalf("unexpected RPC error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions WHERE target = ?`, tc.findingID).Scan(&count); err != nil {
				t.Fatalf("count suppressions: %v", err)
			}
			if count != 1 {
				t.Errorf("expected 1 suppression row, got %d", count)
			}
		})
	}
}

func TestSuppressFinding_MissingParams(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"branch":  "main",
		"repo_id": "repo-1",
		"reason":  "some reason",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
}

// When a single repository is registered, omitting the repository ID automatically defaults to that repository.
func TestListSuppressions_SingleRepoDefaultsRepoID(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	repos := &stubRepoLister{repos: []application.RepoRecord{{RepoID: "only-repo"}}}
	RegisterSuppressionTools(r, db, nil, repos)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchSuppression(t, r, "eng_list_suppressions", actor, map[string]any{
		"branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("expected auto-resolution, got %+v", rpcErr)
	}
}

// Omitting the repository ID when multiple repositories are registered runs the list operation across all repositories.
func TestListSuppressions_MultiRepoDefaultsToFanOut(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "repo-1"},
		{RepoID: "repo-2"},
		{RepoID: "repo-3"},
	}}
	RegisterSuppressionTools(r, db, nil, repos)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchSuppression(t, r, "eng_list_suppressions", actor, map[string]any{})
	if rpcErr != nil {
		t.Fatalf("expected fan-out across repos, got %+v", rpcErr)
	}
}

func TestListSuppressions_Empty(t *testing.T) {
	db := newSuppressionsDB(t)
	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil, nil)

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
	RegisterSuppressionTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-list-001",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accepted",
	})
	if rpcErr != nil {
		t.Fatalf("suppress: %v", rpcErr.Message)
	}

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
