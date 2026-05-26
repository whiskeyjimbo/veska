package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ---------------------------------------------------------------------------
// eng_get_finding (AC1)
// ---------------------------------------------------------------------------

func TestGetFinding(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-get-1", "main", "repo-1")

	r := NewRegistry()
	RegisterRecordTools(r, db, nil)
	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}

	tests := []struct {
		name     string
		params   map[string]any
		wantErr  bool
		wantCode int
		wantID   string
	}{
		{
			name:   "found",
			params: map[string]any{"finding_id": "finding-get-1", "branch": "main"},
			wantID: "finding-get-1",
		},
		{
			name:     "not found",
			params:   map[string]any{"finding_id": "missing", "branch": "main"},
			wantErr:  true,
			wantCode: CodeNotFound,
		},
		{
			// solov2-qwpt: branch is now optional — finding_id is globally
			// unique, so omitting branch resolves the row by id alone.
			name:   "branch omitted resolves by id",
			params: map[string]any{"finding_id": "finding-get-1"},
			wantID: "finding-get-1",
		},
		{
			name:     "wrong branch",
			params:   map[string]any{"finding_id": "finding-get-1", "branch": "other"},
			wantErr:  true,
			wantCode: CodeNotFound,
		},
		{
			// solov2-8kkj: --repo accepted as opt-in scoping assertion.
			name:   "matching repo passes through",
			params: map[string]any{"finding_id": "finding-get-1", "repo_id": "repo-1"},
			wantID: "finding-get-1",
		},
		{
			// solov2-8kkj: --repo with a wrong repo prefix yields NotFound.
			name:     "wrong repo returns not-found",
			params:   map[string]any{"finding_id": "finding-get-1", "repo_id": "different-repo"},
			wantErr:  true,
			wantCode: CodeNotFound,
		},
		{
			// solov2-8kkj: prefix match (short repo id) is accepted.
			name:   "short repo prefix passes through",
			params: map[string]any{"finding_id": "finding-get-1", "repo_id": "repo"},
			wantID: "finding-get-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, rpcErr := dispatchSuppression(t, r, "eng_get_finding", actor, tc.params)
			if tc.wantErr {
				if rpcErr == nil {
					t.Fatal("expected RPC error, got nil")
					return
				}
				if rpcErr.Code != tc.wantCode {
					t.Fatalf("expected code %d, got %d", tc.wantCode, rpcErr.Code)
				}
				return
			}
			if rpcErr != nil {
				t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
			}
			m := result.(map[string]any)
			f := m["finding"].(findingRow)
			if f.FindingID != tc.wantID {
				t.Errorf("expected finding_id %q, got %q", tc.wantID, f.FindingID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// eng_get_suppression (AC1)
// ---------------------------------------------------------------------------

func TestGetSuppression(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-gs-1", "main", "repo-1")

	r := NewRegistry()
	RegisterRecordTools(r, db, nil)
	RegisterSuppressionTools(r, db, nil, nil)
	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}

	res, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-gs-1", "branch": "main", "repo_id": "repo-1", "reason": "fp",
	})
	if rpcErr != nil {
		t.Fatalf("seed suppress: %v", rpcErr.Message)
	}
	supID := res.(map[string]any)["suppression_id"].(string)

	t.Run("found", func(t *testing.T) {
		result, rpcErr := dispatchSuppression(t, r, "eng_get_suppression", actor, map[string]any{
			"suppression_id": supID,
		})
		if rpcErr != nil {
			t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
		}
		s := result.(map[string]any)["suppression"].(suppressionRow)
		if s.SuppressionID != supID {
			t.Errorf("expected %q, got %q", supID, s.SuppressionID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, rpcErr := dispatchSuppression(t, r, "eng_get_suppression", actor, map[string]any{
			"suppression_id": "sup_missing",
		})
		if rpcErr == nil || rpcErr.Code != CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %v", rpcErr)
		}
	})

	t.Run("missing id", func(t *testing.T) {
		_, rpcErr := dispatchSuppression(t, r, "eng_get_suppression", actor, map[string]any{})
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Fatalf("expected CodeInvalidParams, got %v", rpcErr)
		}
	})
}

// ---------------------------------------------------------------------------
// eng_close_suppression (AC2)
// ---------------------------------------------------------------------------

func TestCloseSuppression(t *testing.T) {
	db := newSuppressionsDB(t)
	seedFindingForSuppression(t, db, "finding-cs-1", "main", "repo-1")

	r := NewRegistry()
	RegisterRecordTools(r, db, nil)
	RegisterSuppressionTools(r, db, nil, nil)
	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}

	res, rpcErr := dispatchSuppression(t, r, "eng_suppress_finding", actor, map[string]any{
		"finding_id": "finding-cs-1", "branch": "main", "repo_id": "repo-1", "reason": "temp",
	})
	if rpcErr != nil {
		t.Fatalf("seed suppress: %v", rpcErr.Message)
	}
	supID := res.(map[string]any)["suppression_id"].(string)

	t.Run("closes active suppression", func(t *testing.T) {
		before := time.Now().Unix()
		result, rpcErr := dispatchSuppression(t, r, "eng_close_suppression", actor, map[string]any{
			"suppression_id": supID,
		})
		if rpcErr != nil {
			t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
		}
		after := time.Now().Unix()

		got := result.(map[string]any)["expires_at"].(int64)
		if got < before || got > after {
			t.Errorf("expires_at %d not within [%d,%d]", got, before, after)
		}

		// Verify the row is no longer active (expires_at <= now).
		var expiresAt int64
		if err := db.QueryRow(`SELECT expires_at FROM suppressions WHERE suppression_id = ?`, supID).Scan(&expiresAt); err != nil {
			t.Fatalf("query expires_at: %v", err)
		}
		if expiresAt > time.Now().Unix() {
			t.Errorf("suppression still active: expires_at=%d", expiresAt)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, rpcErr := dispatchSuppression(t, r, "eng_close_suppression", actor, map[string]any{
			"suppression_id": "sup_missing",
		})
		if rpcErr == nil || rpcErr.Code != CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %v", rpcErr)
		}
	})

	t.Run("missing id", func(t *testing.T) {
		_, rpcErr := dispatchSuppression(t, r, "eng_close_suppression", actor, map[string]any{})
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Fatalf("expected CodeInvalidParams, got %v", rpcErr)
		}
	})
}

// ---------------------------------------------------------------------------
// eng_add_repo / eng_remove_repo (AC3)
// ---------------------------------------------------------------------------

// stubRepoRegistrar records calls and lets tests assert on add/remove.
type stubRepoRegistrar struct {
	added   []string
	removed []string
	addID   string
	addErr  error
	rmErr   error
}

func (s *stubRepoRegistrar) AddRepo(_ context.Context, rootPath string) (string, bool, error) {
	s.added = append(s.added, rootPath)
	if s.addErr != nil {
		return "", false, s.addErr
	}
	if s.addID != "" {
		return s.addID, false, nil
	}
	return "repo-" + rootPath, false, nil
}

func (s *stubRepoRegistrar) RemoveRepo(_ context.Context, repoID string) error {
	s.removed = append(s.removed, repoID)
	return s.rmErr
}

func TestAddRepo(t *testing.T) {
	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}

	t.Run("registers and returns", func(t *testing.T) {
		reg := &stubRepoRegistrar{addID: "repo-abc"}
		r := NewRegistry()
		RegisterRepoTools(r, reg)

		result, rpcErr := dispatchSuppression(t, r, "eng_add_repo", actor, map[string]any{
			"root_path": "/tmp/proj",
		})
		if rpcErr != nil {
			t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
		}
		m := result.(map[string]any)
		if m["repo_id"] != "repo-abc" {
			t.Errorf("expected repo_id repo-abc, got %v", m["repo_id"])
		}
		if m["scan_pending"] != true {
			t.Errorf("expected scan_pending true, got %v", m["scan_pending"])
		}
		if len(reg.added) != 1 || reg.added[0] != "/tmp/proj" {
			t.Errorf("expected AddRepo(/tmp/proj), got %v", reg.added)
		}
	})

	t.Run("missing root_path", func(t *testing.T) {
		r := NewRegistry()
		RegisterRepoTools(r, &stubRepoRegistrar{})
		_, rpcErr := dispatchSuppression(t, r, "eng_add_repo", actor, map[string]any{})
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Fatalf("expected CodeInvalidParams, got %v", rpcErr)
		}
	})
}

func TestRemoveRepo(t *testing.T) {
	actor := domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman}

	t.Run("drops repo rows", func(t *testing.T) {
		reg := &stubRepoRegistrar{}
		r := NewRegistry()
		RegisterRepoTools(r, reg)

		result, rpcErr := dispatchSuppression(t, r, "eng_remove_repo", actor, map[string]any{
			"repo_id": "repo-xyz",
		})
		if rpcErr != nil {
			t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
		}
		if result.(map[string]any)["removed"] != true {
			t.Errorf("expected removed true")
		}
		if len(reg.removed) != 1 || reg.removed[0] != "repo-xyz" {
			t.Errorf("expected RemoveRepo(repo-xyz), got %v", reg.removed)
		}
	})

	t.Run("missing repo_id", func(t *testing.T) {
		r := NewRegistry()
		RegisterRepoTools(r, &stubRepoRegistrar{})
		_, rpcErr := dispatchSuppression(t, r, "eng_remove_repo", actor, map[string]any{})
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Fatalf("expected CodeInvalidParams, got %v", rpcErr)
		}
	})
}
