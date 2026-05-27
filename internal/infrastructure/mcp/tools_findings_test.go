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

// capturingAuditWriter records every AuditEntry passed to Write.
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

// newFindingsDBWithEdges adds the edges table to the in-memory DB so auto-link
// promotion tests can verify confidence transitions.
func newFindingsDBWithEdges(t *testing.T) *sql.DB {
	t.Helper()
	db := newFindingsDB(t)
	// Minimal edges table (no FKs — we don't need node rows for promotion tests).
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

// seedAutoLinkFinding inserts a finding row with rule='auto-link' whose node_id
// holds the anchored edge_id.
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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newFindingsDB creates an in-memory SQLite DB seeded with the findings table.
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
	// Minimal suppressions table so eng_list_findings' LEFT JOIN compiles
	// (solov2-2ye2). Tests that don't seed suppressions get the empty-join
	// case automatically.
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
			RegisterFindingTools(r, db, nil, nil)

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
					return
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
	// finding_id and severity are in Data, not Message.
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

// TestCloseFindings_PreservesCreatorActor guards solov2-iyog: closing a
// finding must NOT overwrite actor_id/actor_kind on the row. Those columns
// mean "who created/last-saved this finding"; the closer is recorded
// independently in the audit log. Previously a service-created TODO surfaced
// as actor_id=agent:unknown after any MCP-driven close.
func TestCloseFindings_PreservesCreatorActor(t *testing.T) {
	db := newFindingsDB(t)
	const fid = "finding-actor-preserved-001"
	// Seed with the creator actor — see seedFinding: actor:seed / human.
	seedFinding(t, db, fid, "main", "repo-1", "low", "open")

	r := NewRegistry()
	RegisterFindingTools(r, db, &capturingAuditWriter{}, nil)

	// A human closes it (admin action allowed even at low severity).
	closer := domain.Actor{ID: "agent:unknown", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchFinding(t, r, closer, map[string]string{
		"finding_id": fid,
		"branch":     "main",
		"repo_id":    "repo-1",
		"reason":     "not a real issue",
	})
	if rpcErr != nil {
		// Low severity may still require human; if so, run as human.
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
	// Missing finding_id.
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

// TestListFindings_EmitsDegradedReasonsAsEmptyArray pins solov2-7cw7: the
// README's "Conventions across the tool surface" promises every tool
// includes degraded_reasons (as [] when nothing is degraded). eng_list_findings
// previously omitted the field entirely.
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

// TestListFindings_ResolvesRepoIDFromCWD pins solov2-ig2x: when the caller
// omits repo_id but the shim has injected a cwd that matches a registered
// repo's RootPath, eng_list_findings must resolve via the RepoLister instead
// of erroring "repo_id is required".
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
	// repo_id omitted; cwd hint inside the registered root.
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

// TestListFindings_AcceptsShortID pins solov2-s7k0: eng_list_findings must
// resolve a 12-char short_id prefix the same way the graph tools do, instead
// of querying findings by the raw prefix and silently returning [].
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

	// Short id resolves to the finding.
	result, rpcErr := dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": ShortRepoID(fullID), "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("short_id rejected: %v", rpcErr.Message)
	}
	if findings, _ := result.(map[string]any)["findings"].([]findingRow); len(findings) != 1 {
		t.Fatalf("short_id: want 1 finding, got %d", len(findings))
	}

	// Unknown id errors loudly (not silent empty).
	_, rpcErr = dispatchListFindings(t, r, actor, map[string]string{
		"repo_id": "deadbeef0000", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("unknown repo_id: want CodeNotFound, got %+v", rpcErr)
	}
}

// seedFindingRule inserts a finding with an explicit rule so tests can
// exercise the rule filter (solov2-c7sy).
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

// TestListFindings_RuleFilter pins solov2-c7sy: the rule param must narrow
// results to that rule. Before the fix the param was silently ignored and
// the full unfiltered list came back.
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
	RegisterFindingTools(r, db, nil, nil)

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

// ---------------------------------------------------------------------------
// Auto-link accept-flow tests (m3.04.3)
// ---------------------------------------------------------------------------

// TestCloseFinding_AutoLinkAccept_PromotesEdge verifies the canonical happy
// path: accept on an auto-link finding promotes its anchored edge from
// 'unresolved' to 'definite' in the same tx.
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

	// Verify closed metadata is recorded.
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
	// solov2-iyog: close no longer overwrites actor_id/actor_kind. The row
	// keeps its creator (seeded as actor:seed/agent); the closer is recorded
	// in the audit log via the finding.accept op asserted above.
	if actorID != "actor:seed" || actorKind != "agent" {
		t.Errorf("expected creator actor preserved (actor:seed/agent), got %s/%s", actorID, actorKind)
	}
}

// TestCloseFinding_AutoLinkSuppress_LeavesEdgeUnresolved confirms that the
// suppress-flow on an auto-link finding does NOT touch the edge.
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

// TestCloseFinding_NonAutoLinkAccept_LeavesEdgesAlone verifies that accept on a
// non-auto-link rule does not promote anything. We use a distinct rule and
// ensure that even an edge sharing the finding's node_id is untouched.
func TestCloseFinding_NonAutoLinkAccept_LeavesEdgesAlone(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	// Seed an edge whose id matches what could be misread as an anchor —
	// this guards against accidental promotion regardless of rule.
	seedEdge(t, db, "node-123", "main", "repo-1", "unresolved")

	// Seed a regular finding with rule='dead-code'.
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

// TestCloseFinding_AutoLinkAccept_NullAnchor closes a finding without crashing
// when the node_id (anchor) is NULL. No edge promotion runs.
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

// TestCloseFinding_AutoLinkAccept_MissingEdge documents the soft-fail policy:
// when the anchor points to an edge_id that does not exist, the UPDATE affects
// zero rows but the transaction commits, so the finding still closes.
func TestCloseFinding_AutoLinkAccept_MissingEdge(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	// Note: no seedEdge call — anchor points at a missing edge.
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

// TestCloseFinding_AutoLinkAccept_AlreadyDefinite verifies idempotency: an
// already-definite edge stays definite without error.
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

// TestCloseFinding_AutoLinkAccept_AtomicRollback proves the finding-close and
// edge-promote share a transaction: forcing the edges table into a state where
// the promotion UPDATE fails must leave the finding in state='open'.
//
// We trigger a failure by dropping the edges table so the UPDATE returns a
// "no such table" error AFTER the SELECT but BEFORE the finding UPDATE.
func TestCloseFinding_AutoLinkAccept_AtomicRollback(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedAutoLinkFinding(t, db, "f-al-rb", "main", "repo-1", "low", "open", "edge-x")

	// Force the next UPDATE edges to fail.
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

	// Finding must remain open because the tx rolled back.
	if got := findingState(t, db, "f-al-rb", "main"); got != "open" {
		t.Errorf("expected finding to roll back to open, got %q", got)
	}

	// No audit entry on failure.
	if ops := aw.ops(); len(ops) != 0 {
		t.Errorf("expected no audit entries on rollback, got %v", ops)
	}
}

// TestCloseFinding_HumanGate_AcceptPath confirms the human-action gate still
// applies when reason=accept on an auto-link finding.
func TestCloseFinding_HumanGate_AcceptPath(t *testing.T) {
	db := newFindingsDBWithEdges(t)
	seedEdge(t, db, "edge-h", "main", "repo-1", "unresolved")
	seedAutoLinkFinding(t, db, "f-al-h", "main", "repo-1", "high", "open", "edge-h")

	r := NewRegistry()
	RegisterFindingTools(r, db, nil, nil)

	// Agent attempts to accept a high-severity auto-link finding.
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

	// Finding remains open, edge remains unresolved.
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
		// missing finding_id
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing finding_id")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

// ---------------------------------------------------------------------------
// review-pipeline-failure: close-flips-row contract (solov2-nz2.3 AC2)
// ---------------------------------------------------------------------------

// newFindingsDBWithQueue adds the post_promotion_queue table so the
// review-pipeline-failure close path can flip a failed review row to done.
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

// seedReviewSemanticFinding inserts a review-produced finding row: a
// file-anchored finding carrying source_layer='semantic', a review-* rule, and
// actor_kind='system' — the row shape the review Handler persists (nz2.6).
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

// TestListFindings_SurfacesReviewSemanticFinding verifies AC1: a review-produced
// finding (source_layer='semantic') is returned by eng_list_findings like any
// structural finding.
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

// TestSuppressFinding_ReviewSemanticFinding verifies AC2: a review finding is
// suppressible via eng_suppress_finding like any structural finding.
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

// TestCloseFinding_ReviewSemanticFinding_HumanGate verifies AC3: closing a
// high-severity review finding is refused for a non-human actor and succeeds
// for a human actor.
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

// TestCloseFinding_ReviewFailure_FlipsRowToDone verifies AC2: closing a
// review-pipeline-failure finding flips the anchored failed review row(s) to
// state='done' in the same tx.
func TestCloseFinding_ReviewFailure_FlipsRowToDone(t *testing.T) {
	db := newFindingsDBWithQueue(t)
	const gitSHA = "sha-deadbeef"
	// Two failed review files in one commit collapse to one finding.
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

// TestCloseFinding_ReviewFailure_NonHumanRejected confirms the human-action
// gate fires for the high-severity review-pipeline-failure finding before any
// row is flipped.
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
