// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

type capturingAuditWriter struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (c *capturingAuditWriter) Write(_ context.Context, e ports.AuditEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
	return nil
}

func (c *capturingAuditWriter) ops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.entries))
	for i, e := range c.entries {
		out[i] = e.Op
	}
	return out
}

func newFindingsDBWithEdges(t *testing.T) *sql.DB {
	t.Helper()
	db := newFindingsDB(t)
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS edges (
			edge_id          TEXT NOT NULL,
			branch           TEXT NOT NULL,
			repo_id          TEXT NOT NULL,
			src_node_id      TEXT NOT NULL,
			dst_node_id      TEXT NOT NULL,
			kind             TEXT NOT NULL,
			confidence       TEXT NOT NULL,
			last_promoted_at INTEGER NOT NULL,
			PRIMARY KEY (edge_id, branch)
		)
	`)
	if err != nil {
		t.Fatalf("create edges table: %v", err)
	}
	return db
}

func seedAutoLinkFinding(t *testing.T, db *sql.DB, findingID, branch, repoID, severity, state, edgeID string) {
	t.Helper()
	var nodeID any
	if edgeID == "" {
		nodeID = nil
	} else {
		nodeID = edgeID
	}
	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, 'main.go', ?, 'semantic', 'auto-link', 'autolink candidate', ?, ?, 'actor:seed', 'agent')
	`, findingID, branch, repoID, nodeID, severity, state, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed auto-link finding: %v", err)
	}
}

func seedEdge(t *testing.T, db *sql.DB, edgeID, branch, repoID, confidence string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO edges (edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at)
		VALUES (?, ?, ?, 'n:src', 'n:dst', 'call', ?, ?)
	`, edgeID, branch, repoID, confidence, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed edge: %v", err)
	}
}

func edgeConfidence(t *testing.T, db *sql.DB, edgeID, branch string) string {
	t.Helper()
	var c string
	err := db.QueryRow(`SELECT confidence FROM edges WHERE edge_id = ? AND branch = ?`, edgeID, branch).Scan(&c)
	if err != nil {
		t.Fatalf("query edge confidence: %v", err)
	}
	return c
}

func findingState(t *testing.T, db *sql.DB, findingID, branch string) string {
	t.Helper()
	var s string
	err := db.QueryRow(`SELECT state FROM findings WHERE finding_id = ? AND branch = ?`, findingID, branch).Scan(&s)
	if err != nil {
		t.Fatalf("query finding state: %v", err)
	}
	return s
}

// newFindingsDB initializes an in-memory SQLite database seeded with minimal findings, suppressions, and nodes tables.
func newFindingsDB(t *testing.T) *sql.DB {
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
			actor_kind     TEXT NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create suppressions table: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS nodes (
			node_id     TEXT NOT NULL,
			branch      TEXT NOT NULL,
			file_path   TEXT NOT NULL,
			PRIMARY KEY (node_id, branch)
		)
	`)
	if err != nil {
		t.Fatalf("create nodes table: %v", err)
	}
	return db
}

func seedNodeAnchoredFinding(t *testing.T, db *sql.DB, findingID, branch, repoID, nodeID, nodeFile string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO nodes (node_id, branch, file_path) VALUES (?, ?, ?)
	`, nodeID, branch, nodeFile); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, NULL, 'low', 'structural', 'dead-code', 'unused symbol', 'open', ?, 'actor:seed', 'system')
	`, findingID, branch, repoID, nodeID, time.Now().Unix()); err != nil {
		t.Fatalf("seed node-anchored finding: %v", err)
	}
}

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

func TestCloseFindings_HumanActionGate(t *testing.T) {
	tests := []struct {
		name       string
		severity   string
		actor      domain.Actor
		wantCode   int
		wantReason string
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
			RegisterFindingTools(r, db, nil, nil)

			result, rpcErr := dispatchFinding(t, r, tc.actor, map[string]string{
				"finding_id": findingID,
				"branch":     "main",
				"repo_id":    "repo-1",
				"reason":     "resolved",
			})

			if tc.wantCode != 0 {
				if rpcErr == nil {
					t.Fatalf("expected RPC error with code %d, got result: %v", tc.wantCode, result)
					return
				}
				if rpcErr.Code != tc.wantCode {
					t.Errorf("expected code %d, got %d", tc.wantCode, rpcErr.Code)
				}
				if tc.wantReason != "" && !strings.Contains(rpcErr.Message, tc.wantReason) {
					t.Errorf("expected message to contain %q, got %q", tc.wantReason, rpcErr.Message)
				}
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

			if rpcErr != nil {
				t.Fatalf("unexpected RPC error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
			}

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
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": fid,
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "resolved",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error")
		return
	}
	data, ok := rpcErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected RPCError.Data to be map[string]any, got %T", rpcErr.Data)
	}
	if data["finding_id"] != fid {
		t.Errorf("expected finding_id %q in Data, got %v", fid, data["finding_id"])
	}
	if data["severity"] != "critical" {
		t.Errorf("expected severity=critical in Data, got %v", data["severity"])
	}
}

// The closing operation must not overwrite the creator actor fields on the finding record because the closer identity is tracked in the audit log.
func TestCloseFindings_PreservesCreatorActor(t *testing.T) {
	db := newFindingsDB(t)
	const fid = "finding-actor-preserved-001"
	seedFinding(t, db, fid, "main", "repo-1", "low", "open")

	r := NewRegistry()
	RegisterFindingTools(r, db, &capturingAuditWriter{}, nil)

	closer := domain.Actor{ID: "agent:unknown", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchFinding(t, r, closer, map[string]string{
		"finding_id": fid,
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "not a real issue",
	})
	if rpcErr != nil {
		closer = domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
		_, rpcErr = dispatchFinding(t, r, closer, map[string]string{
			"finding_id": fid,
			"branch":     "main",
			"repo_id":    "repo-1",
			"reason":     "not a real issue",
		})
		if rpcErr != nil {
			t.Fatalf("dispatch (human): %+v", rpcErr)
		}
	}

	var aid, akind string
	if err := db.QueryRow(`SELECT actor_id, actor_kind FROM findings WHERE finding_id = ? AND branch = ?`, fid, "main").Scan(&aid, &akind); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if aid != "actor:seed" || akind != "human" {
		t.Errorf("creator actor clobbered by close: actor_id=%q actor_kind=%q want actor:seed/human", aid, akind)
	}
}

func TestCloseFindings_NotFound(t *testing.T) {
	db := newFindingsDB(t)

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "no-such-finding",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "resolved",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error for not-found finding")
		return
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected code %d, got %d", CodeNotFound, rpcErr.Code)
	}
}

func TestCloseFindings_MissingParams(t *testing.T) {
	db := newFindingsDB(t)

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"branch":  "main",
		"repo_id": "repo-1",
		"reason":  "resolved",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
}

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
	RegisterFindingTools(r, db, nil, nil)

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

// The response must include an empty degraded_reasons array by default to adhere to the tool surface contract.
func TestListFindings_EmitsDegradedReasonsAsEmptyArray(t *testing.T) {
	db := newFindingsDB(t)
	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr.Message)
	}
	b, _ := json.Marshal(result)
	if !strings.Contains(string(b), `"degraded_reasons":[]`) {
		t.Errorf("expected degraded_reasons:[] in JSON, got: %s", string(b))
	}
}

// For node-anchored findings that lack their own file path, we resolve the path by joining with the referenced graph node record.
func TestListFindings_ResolvesFilePathFromNodeAnchor(t *testing.T) {
	db := newFindingsDB(t)
	seedNodeAnchoredFinding(t, db, "dc-1", "main", "repo-1", "node-abc", "internal/foo/bar.go")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)
	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}

	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr.Message)
	}
	m, _ := result.(map[string]any)
	rows, _ := m["findings"].([]findingRow)
	if len(rows) != 1 {
		t.Fatalf("expected 1 finding; got %d", len(rows))
	}
	if rows[0].FilePath == nil {
		t.Fatalf("expected resolved FilePath for node-anchored finding; got nil")
	}
	if *rows[0].FilePath != "internal/foo/bar.go" {
		t.Errorf("FilePath mismatch: want internal/foo/bar.go, got %q", *rows[0].FilePath)
	}
}

func TestListFindings_DefaultStateIsOpen(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "open-f", "main", "repo-1", "low", "open")
	seedFinding(t, db, "closed-f", "main", "repo-1", "low", "closed")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1",
		"branch":  "main",
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

// Querying with state='any' returns findings in all lifecycle states (open, closed, suppressed) in a single request.
func TestListFindings_StateAnyReturnsEveryLifecycleRow(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "open-f", "main", "repo-1", "low", "open")
	seedFinding(t, db, "closed-f", "main", "repo-1", "low", "closed")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1", "branch": "main", "state": "any",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}
	m := result.(map[string]any)
	findings, _ := m["findings"].([]findingRow)
	if len(findings) != 2 {
		t.Errorf("expected 2 findings (open + closed), got %d: %+v", len(findings), findings)
	}
}

// If repo_id is omitted but the current working directory matches a registered repository's root path, we automatically resolve the repo ID.
func TestListFindings_ResolvesRepoIDFromCWD(t *testing.T) {
	const fullID = "62d72fa222a0193f8fa927f95dd6a3575c7566964c8b8f6ba14aafc5a1ea871f"
	db := newFindingsDB(t)
	seedFinding(t, db, "f-cwd", "main", fullID, "low", "open")

	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: fullID, ActiveBranch: "main", RootPath: "/tmp/myrepo"},
	}}
	r := NewRegistry()
	RegisterFindingTools(r, db, nil, repos)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"cwd": "/tmp/myrepo/subdir",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
	}
	m := result.(map[string]any)
	findings, _ := m["findings"].([]findingRow)
	if len(findings) != 1 || findings[0].FindingID != "f-cwd" {
		t.Errorf("expected the cwd-resolved finding, got %+v", findings)
	}
}

// We resolve short repository ID prefixes when listing findings to match the behavior of graph query tools.
func TestListFindings_AcceptsShortID(t *testing.T) {
	const fullID = "62d72fa222a0193f8fa927f95dd6a3575c7566964c8b8f6ba14aafc5a1ea871f"
	db := newFindingsDB(t)
	if _, err := db.Exec(`CREATE TABLE repos (repo_id TEXT PRIMARY KEY, root_path TEXT, added_at INTEGER)`); err != nil {
		t.Fatalf("create repos: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, '/tmp/r', 0)`, fullID); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	seedFinding(t, db, "f1", "main", fullID, "low", "open")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)
	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}

	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": ShortRepoID(fullID), "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("short_id rejected: %v", rpcErr.Message)
	}
	if findings, _ := result.(map[string]any)["findings"].([]findingRow); len(findings) != 1 {
		t.Fatalf("short_id: want 1 finding, got %d", len(findings))
	}

	_, rpcErr = dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "deadbeef0000", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("unknown repo_id: want CodeNotFound, got %+v", rpcErr)
	}
}

func seedFindingRule(t *testing.T, db *sql.DB, findingID, branch, repoID, severity, state, rule string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, NULL, 'main.go', ?, 'security', ?, 'test message', ?, ?, 'actor:seed', 'human')
	`, findingID, branch, repoID, severity, rule, state, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
}

// The rule parameter narrows the findings list to matching rule names.
func TestListFindings_RuleFilter(t *testing.T) {
	db := newFindingsDB(t)
	seedFindingRule(t, db, "vuln-f", "main", "repo-1", "high", "open", "vuln")
	seedFindingRule(t, db, "dead-f", "main", "repo-1", "low", "open", "dead-code")
	seedFindingRule(t, db, "secret-f", "main", "repo-1", "high", "open", "secret_leak")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1",
		"branch":  "main",
		"rule":    "vuln",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}
	m := result.(map[string]any)
	findings, _ := m["findings"].([]findingRow)
	if len(findings) != 1 {
		t.Fatalf("expected 1 vuln finding, got %d", len(findings))
	}
	if findings[0].Rule != "vuln" {
		t.Errorf("expected rule=vuln, got %q", findings[0].Rule)
	}
}

func TestListFindings_SeverityFilter(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "low-f", "main", "repo-1", "low", "open")
	seedFinding(t, db, "high-f", "main", "repo-1", "high", "open")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

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
	RegisterFindingTools(r, db, nil, nil)

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

	var state string
	if err := db.QueryRow(`SELECT state FROM findings WHERE finding_id = 'reopen-f' AND branch = 'main'`).Scan(&state); err != nil {
		t.Fatalf("query state: %v", err)
	}
	if state != "open" {
		t.Errorf("expected state=open in DB, got %q", state)
	}
}

// An optional reopen reason is captured and recorded in the audit log for tracking purposes.
func TestReopenFinding_RecordsReasonInAudit(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "reopen-reason", "main", "repo-1", "low", "closed")

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"finding_id": "reopen-reason",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "false positive on second look",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	var found bool
	for _, e := range aw.entries {
		if e.Op == "finding.reopen" {
			found = true
			if e.Reason != "false positive on second look" {
				t.Errorf("audit reason = %q, want the supplied reopen reason", e.Reason)
			}
		}
	}
	if !found {
		t.Fatal("no finding.reopen audit entry written")
	}
}

// Any actor is permitted to reopen a finding, as the human-only safety gate only restricts closures.
func TestReopenFinding_AnyActorCanReopen(t *testing.T) {
	db := newFindingsDB(t)
	seedFinding(t, db, "reopen-agent", "main", "repo-1", "critical", "closed")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

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
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"finding_id": "no-such",
		"branch":     "main",
		"repo_id":    "repo-1",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for not-found finding")
		return
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected CodeNotFound, got %d", rpcErr.Code)
	}
}

// Accepting an auto-link finding promotes the associated unresolved edge to definite confidence in the same transaction.
func TestCloseFinding_AutoLinkAccept_PromotesEdge(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedEdge(t, db, "edge-1", "main", "repo-1", "unresolved")
	seedAutoLinkFinding(t, db, "f-al-1", "main", "repo-1", "low", "open", "edge-1")

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-1",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	if got := findingState(t, db, "f-al-1", "main"); got != "closed" {
		t.Errorf("expected finding state=closed, got %q", got)
	}
	if got := edgeConfidence(t, db, "edge-1", "main"); got != "definite" {
		t.Errorf("expected edge confidence=definite, got %q", got)
	}

	ops := aw.ops()
	if len(ops) != 1 || ops[0] != "finding.accept" {
		t.Errorf("expected single audit op=finding.accept, got %v", ops)
	}

	var (
		closedReason sql.NullString
		closedAt     sql.NullInt64
		actorID      string
		actorKind    string
	)
	if err := db.QueryRow(
		`SELECT closed_reason, closed_at, actor_id, actor_kind FROM findings WHERE finding_id = 'f-al-1' AND branch = 'main'`,
	).Scan(&closedReason, &closedAt, &actorID, &actorKind); err != nil {
		t.Fatalf("query close metadata: %v", err)
	}
	if !closedReason.Valid || closedReason.String != "accept" {
		t.Errorf("expected closed_reason=accept, got %v", closedReason)
	}
	if !closedAt.Valid || closedAt.Int64 == 0 {
		t.Errorf("expected closed_at to be set, got %v", closedAt)
	}
	if actorID != "actor:seed" || actorKind != "agent" {
		t.Errorf("expected creator actor preserved (actor:seed/agent), got %s/%s", actorID, actorKind)
	}
}

// Suppressing an auto-link finding closes the finding but leaves the associated edge in its unresolved state.
func TestCloseFinding_AutoLinkSuppress_LeavesEdgeUnresolved(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedEdge(t, db, "edge-2", "main", "repo-1", "unresolved")
	seedAutoLinkFinding(t, db, "f-al-2", "main", "repo-1", "low", "open", "edge-2")

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-2",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "suppress",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	if got := findingState(t, db, "f-al-2", "main"); got != "closed" {
		t.Errorf("expected finding state=closed, got %q", got)
	}
	if got := edgeConfidence(t, db, "edge-2", "main"); got != "unresolved" {
		t.Errorf("expected edge confidence to remain unresolved, got %q", got)
	}

	ops := aw.ops()
	if len(ops) != 1 || ops[0] != "finding.close" {
		t.Errorf("expected audit op=finding.close on suppress, got %v", ops)
	}
}

// Accepting non-auto-link findings must not promote any edges, even if the node IDs match.
func TestCloseFinding_NonAutoLinkAccept_LeavesEdgesAlone(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedEdge(t, db, "node-123", "main", "repo-1", "unresolved")

	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES ('f-dc-1', 'main', 'repo-1', 'node-123', 'main.go', 'low', 'structural', 'dead-code', 'unused', 'open', ?, 'actor:seed', 'agent')
	`, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed dead-code finding: %v", err)
	}

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-dc-1",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	if got := findingState(t, db, "f-dc-1", "main"); got != "closed" {
		t.Errorf("expected finding state=closed, got %q", got)
	}
	if got := edgeConfidence(t, db, "node-123", "main"); got != "unresolved" {
		t.Errorf("expected non-auto-link rule to leave edge alone; got confidence=%q", got)
	}

	ops := aw.ops()
	if len(ops) != 1 || ops[0] != "finding.close" {
		t.Errorf("expected audit op=finding.close for non-auto-link rule, got %v", ops)
	}
}

// Accepting an auto-link finding with a null node ID closes the finding without executing edge promotion.
func TestCloseFinding_AutoLinkAccept_NullAnchor(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedAutoLinkFinding(t, db, "f-al-null", "main", "repo-1", "low", "open", "")

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-null",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}
	if got := findingState(t, db, "f-al-null", "main"); got != "closed" {
		t.Errorf("expected finding to close, got state=%q", got)
	}
}

// If the referenced edge ID does not exist in the database, the accept operation soft-fails and proceeds to close the finding.
func TestCloseFinding_AutoLinkAccept_MissingEdge(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedAutoLinkFinding(t, db, "f-al-orphan", "main", "repo-1", "low", "open", "edge-does-not-exist")

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-orphan",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error for missing-edge soft-fail: %v", rpcErr.Message)
	}
	if got := findingState(t, db, "f-al-orphan", "main"); got != "closed" {
		t.Errorf("expected finding to close despite missing edge, got state=%q", got)
	}

	ops := aw.ops()
	if len(ops) != 1 || ops[0] != "finding.accept" {
		t.Errorf("expected audit op=finding.accept on accept-path (even with missing edge), got %v", ops)
	}
}

// Accepting an auto-link finding when the edge is already definite succeeds idempotently.
func TestCloseFinding_AutoLinkAccept_AlreadyDefinite(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedEdge(t, db, "edge-def", "main", "repo-1", "definite")
	seedAutoLinkFinding(t, db, "f-al-idem", "main", "repo-1", "low", "open", "edge-def")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-idem",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}
	if got := findingState(t, db, "f-al-idem", "main"); got != "closed" {
		t.Errorf("expected finding to close, got state=%q", got)
	}
	if got := edgeConfidence(t, db, "edge-def", "main"); got != "definite" {
		t.Errorf("expected edge to remain definite, got %q", got)
	}
}

// The finding closure and edge promotion are executed within a shared transaction, so a failure in the promotion step rolls back the closure.
func TestCloseFinding_AutoLinkAccept_AtomicRollback(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedAutoLinkFinding(t, db, "f-al-rb", "main", "repo-1", "low", "open", "edge-x")

	if _, err := db.Exec(`DROP TABLE edges`); err != nil {
		t.Fatalf("drop edges: %v", err)
	}

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-rb",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error from failed edge UPDATE")
		return
	}
	if rpcErr.Code != CodeInternalError {
		t.Errorf("expected CodeInternalError, got %d", rpcErr.Code)
	}

	if got := findingState(t, db, "f-al-rb", "main"); got != "open" {
		t.Errorf("expected finding to roll back to open, got %q", got)
	}

	if ops := aw.ops(); len(ops) != 0 {
		t.Errorf("expected no audit entries on rollback, got %v", ops)
	}
}

// The human-only action gate applies when accepting auto-link findings.
func TestCloseFinding_HumanGate_AcceptPath(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedEdge(t, db, "edge-h", "main", "repo-1", "unresolved")
	seedAutoLinkFinding(t, db, "f-al-h", "main", "repo-1", "high", "open", "edge-h")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:claude", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-al-h",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accept",
	})
	if rpcErr == nil {
		t.Fatal("expected human_required error")
		return
	}
	if rpcErr.Code != CodeHumanRequired {
		t.Errorf("expected CodeHumanRequired, got %d", rpcErr.Code)
	}

	if got := findingState(t, db, "f-al-h", "main"); got != "open" {
		t.Errorf("expected finding to remain open, got %q", got)
	}
	if got := edgeConfidence(t, db, "edge-h", "main"); got != "unresolved" {
		t.Errorf("expected edge to remain unresolved, got %q", got)
	}
}

func TestReopenFinding_MissingParams(t *testing.T) {
	db := newFindingsDB(t)
	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchReopenFinding(t, r, actor, map[string]string{
		"branch":  "main",
		"repo_id": "repo-1",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

// newFindingsDBWithQueue initializes an in-memory database containing the post_promotion_queue table.
func newFindingsDBWithQueue(t *testing.T) *sql.DB {
	t.Helper()
	db := newFindingsDB(t)
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS post_promotion_queue (
			seq          INTEGER PRIMARY KEY AUTOINCREMENT,
			promotion_id TEXT NOT NULL,
			repo_id      TEXT NOT NULL,
			branch       TEXT NOT NULL,
			git_sha      TEXT NOT NULL,
			work_kind    TEXT NOT NULL,
			payload      TEXT NOT NULL,
			state        TEXT NOT NULL,
			attempts     INTEGER NOT NULL DEFAULT 0,
			enqueued_at  INTEGER NOT NULL,
			completed_at INTEGER,
			error        TEXT
		)
	`)
	if err != nil {
		t.Fatalf("create post_promotion_queue table: %v", err)
	}
	return db
}

func seedReviewRow(t *testing.T, db *sql.DB, repoID, branch, gitSHA, payload, state string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, attempts, enqueued_at)
		VALUES ('promo-1', ?, ?, ?, 'review', ?, ?, 3, ?)
	`, repoID, branch, gitSHA, payload, state, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed review row: %v", err)
	}
}

func seedReviewFailureFinding(t *testing.T, db *sql.DB, findingID, branch, repoID, gitSHA string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, ?, NULL, 'high', 'quality', 'review-pipeline-failure', 'review pipeline failed', 'open', ?, 'service:veska', 'system')
	`, findingID, branch, repoID, gitSHA, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed review-pipeline-failure finding: %v", err)
	}
}

func reviewRowState(t *testing.T, db *sql.DB, repoID, branch, gitSHA string) string {
	t.Helper()
	var s string
	err := db.QueryRow(
		`SELECT state FROM post_promotion_queue WHERE work_kind='review' AND repo_id=? AND branch=? AND git_sha=?`,
		repoID, branch, gitSHA,
	).Scan(&s)
	if err != nil {
		t.Fatalf("query review row state: %v", err)
	}
	return s
}

func seedReviewSemanticFinding(t *testing.T, db *sql.DB, findingID, branch, repoID, rule, severity string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
		VALUES (?, ?, ?, NULL, 'pkg/svc/auth.go', ?, 'semantic', ?, 'review finding', 'open', ?, 'service:veska', 'system')
	`, findingID, branch, repoID, severity, rule, time.Now().Unix())
	if err != nil {
		t.Fatalf("seed review semantic finding: %v", err)
	}
}

// We list review-produced semantic findings in the response along with other findings.
func TestListFindings_SurfacesReviewSemanticFinding(t *testing.T) {
	db := newFindingsDB(t)
	seedReviewSemanticFinding(t, db, "f-rev-sec", "main", "repo-1", "review-security", "high")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "repo-1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}
	m := result.(map[string]any)
	findings, _ := m["findings"].([]findingRow)
	if len(findings) != 1 {
		t.Fatalf("expected 1 review finding listed, got %d", len(findings))
	}
	if findings[0].SourceLayer != "semantic" {
		t.Errorf("source_layer = %q, want semantic", findings[0].SourceLayer)
	}
	if findings[0].Rule != "review-security" {
		t.Errorf("rule = %q, want review-security", findings[0].Rule)
	}
}

// Review-produced semantic findings are suppressible in the same manner as structural findings.
func TestSuppressFinding_ReviewSemanticFinding(t *testing.T) {
	db := newSuppressionsDB(t)
	seedReviewSemanticFinding(t, db, "f-rev-cd", "main", "repo-1", "review-contract-drift", "medium")

	r := NewRegistry()
	RegisterSuppressionTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "f-rev-cd",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "accepted risk",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
	}
	supID, _ := result.(map[string]any)["suppression_id"].(string)
	if supID == "" {
		t.Fatal("expected non-empty suppression_id for a suppressed review finding")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suppressions WHERE suppression_id = ?`, supID).Scan(&count); err != nil {
		t.Fatalf("query suppression: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 suppression row, got %d", count)
	}
}

// The human closure gate enforces restriction on closing high-severity review findings.
func TestCloseFinding_ReviewSemanticFinding_HumanGate(t *testing.T) {
	t.Run("agent refused", func(t *testing.T) {
		db := newFindingsDB(t)
		seedReviewSemanticFinding(t, db, "f-rev-hi", "main", "repo-1", "review-security", "high")
		r := NewRegistry()
		RegisterFindingTools(r, db, nil, nil)

		actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
		_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
			"finding_id": "f-rev-hi",
			"branch":     "main",
			"repo_id":    "repo-1",
			"reason":     "resolved",
		})
		if rpcErr == nil || rpcErr.Code != CodeHumanRequired {
			t.Fatalf("expected human-required RPC error, got %v", rpcErr)
		}
		if got := findingState(t, db, "f-rev-hi", "main"); got != "open" {
			t.Errorf("expected finding to remain open, got %q", got)
		}
	})

	t.Run("human accepted", func(t *testing.T) {
		db := newFindingsDB(t)
		seedReviewSemanticFinding(t, db, "f-rev-hi2", "main", "repo-1", "review-security", "high")
		r := NewRegistry()
		RegisterFindingTools(r, db, nil, nil)

		actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
		_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
			"finding_id": "f-rev-hi2",
			"branch":     "main",
			"repo_id":    "repo-1",
			"reason":     "resolved",
		})
		if rpcErr != nil {
			t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
		}
		if got := findingState(t, db, "f-rev-hi2", "main"); got != "closed" {
			t.Errorf("expected finding state=closed, got %q", got)
		}
	})
}

// Closing a review pipeline failure finding updates the status of the associated failed review rows to done.
func TestCloseFinding_ReviewFailure_FlipsRowToDone(t *testing.T) {
	db := newFindingsDBWithQueue(t)
	const gitSHA = "sha-deadbeef"
	seedReviewRow(t, db, "repo-1", "main", gitSHA, "a.go", "failed")
	seedReviewRow(t, db, "repo-1", "main", gitSHA, "b.go", "failed")
	seedReviewFailureFinding(t, db, "f-rpf-1", "main", "repo-1", gitSHA)

	aw := &capturingAuditWriter{}
	r := NewRegistry()
	RegisterFindingTools(r, db, aw, nil)

	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-rpf-1",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "resolved",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}
	if got := findingState(t, db, "f-rpf-1", "main"); got != "closed" {
		t.Errorf("expected finding state=closed, got %q", got)
	}

	var done int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM post_promotion_queue WHERE work_kind='review' AND repo_id='repo-1' AND branch='main' AND git_sha=? AND state='done'`,
		gitSHA,
	).Scan(&done); err != nil {
		t.Fatalf("count done rows: %v", err)
	}
	if done != 2 {
		t.Errorf("expected both failed review rows flipped to done, got %d", done)
	}
}

// A non-human actor is blocked from closing high-severity review failure findings.
func TestCloseFinding_ReviewFailure_NonHumanRejected(t *testing.T) {
	db := newFindingsDBWithQueue(t)
	const gitSHA = "sha-cafe"
	seedReviewRow(t, db, "repo-1", "main", gitSHA, "a.go", "failed")
	seedReviewFailureFinding(t, db, "f-rpf-2", "main", "repo-1", gitSHA)

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchFinding(t, r, actor, map[string]string{
		"finding_id": "f-rpf-2",
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "resolved",
	})
	if rpcErr == nil || rpcErr.Code != CodeHumanRequired {
		t.Fatalf("expected human-required RPC error, got %v", rpcErr)
	}
	if got := reviewRowState(t, db, "repo-1", "main", gitSHA); got != "failed" {
		t.Errorf("expected review row to remain failed, got %q", got)
	}
}

func TestRelativizeFindingPath(t *testing.T) {
	root := "/tmp/mycli"
	abs := "/tmp/mycli/internal/server/server.go"
	rel := "internal/server/secrets.go"

	if got := relativizeFindingPath(&abs, root); got == nil || *got != "internal/server/server.go" {
		t.Errorf("absolute under root: got %v, want internal/server/server.go", got)
	}
	if got := relativizeFindingPath(&rel, root); got == nil || *got != rel {
		t.Errorf("already-relative left untouched: got %v, want %q", got, rel)
	}
	if got := relativizeFindingPath(nil, root); got != nil {
		t.Errorf("nil path (auto-link) must stay nil, got %v", got)
	}
	outside := "/etc/passwd"
	if got := relativizeFindingPath(&outside, root); got == nil || *got != outside {
		t.Errorf("path outside root left untouched: got %v", got)
	}
}
