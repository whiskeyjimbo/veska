package mcp

import (
	"context"
	"encoding/json"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ---------------------------------------------------------------------------
// Stub RepoLister
// ---------------------------------------------------------------------------

type stubRepoLister struct {
	repos []application.RepoRecord
	err   error
}

func (s *stubRepoLister) ListRepos(_ context.Context) ([]application.RepoRecord, error) {
	return s.repos, s.err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func dispatchAdmin(t *testing.T, r *Registry, method string, params any) (any, *RPCError) {
	t.Helper()
	req := &Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  mustMarshal(t, params),
	}
	return r.Dispatch(context.Background(), domain.ActorKindAgent, req)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

var sampleRepos = []application.RepoRecord{
	{RepoID: "repo-1", RootPath: "/home/user/project", ActiveBranch: "main", LastPromotedSHA: "abc123"},
	{RepoID: "repo-2", RootPath: "/home/user/other", ActiveBranch: "dev", LastPromotedSHA: "def456"},
}

func TestAdminTools_GetCurrentRepo_Found(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_get_current_repo", map[string]string{
		"cwd": "/home/user/project/internal/foo",
	})

	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any, got %T", result)
	}

	repo, ok := m["repo"].(application.RepoRecord)
	if !ok {
		t.Fatalf("result[\"repo\"] is not RepoRecord, got %T", m["repo"])
	}
	if repo.RepoID != "repo-1" {
		t.Errorf("expected repo-1, got %q", repo.RepoID)
	}

	degraded, ok := m["degraded_reasons"].([]string)
	if !ok {
		t.Fatalf("degraded_reasons missing or wrong type: %T", m["degraded_reasons"])
	}
	if len(degraded) != 0 {
		t.Errorf("expected empty degraded_reasons, got %v", degraded)
	}

	if m["included_staging"] != true {
		t.Errorf("expected included_staging=true")
	}
}

func TestAdminTools_GetCurrentRepo_NotFound(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	_, rpcErr := dispatchAdmin(t, r, "eng_get_current_repo", map[string]string{
		"cwd": "/tmp/unrelated/path",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code -32602, got %d", rpcErr.Code)
	}
}

func TestAdminTools_GetCurrentRepo_MissingCwd(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	_, rpcErr := dispatchAdmin(t, r, "eng_get_current_repo", map[string]string{})

	if rpcErr == nil {
		t.Fatal("expected RPC error for missing cwd")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected -32602, got %d", rpcErr.Code)
	}
}

func TestAdminTools_ListRepos(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_list_repos", map[string]any{})

	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any, got %T", result)
	}

	repos, ok := m["repos"].([]application.RepoRecord)
	if !ok {
		t.Fatalf("repos missing or wrong type: %T", m["repos"])
	}
	if len(repos) != 2 {
		t.Errorf("expected 2 repos, got %d", len(repos))
	}

	degraded, ok := m["degraded_reasons"].([]string)
	if !ok {
		t.Fatalf("degraded_reasons missing or wrong type")
	}
	if len(degraded) != 0 {
		t.Errorf("expected empty degraded_reasons")
	}
}

func TestAdminTools_GetRepo_Found(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_get_repo", map[string]string{
		"repo_id": "repo-2",
	})

	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any, got %T", result)
	}

	repo, ok := m["repo"].(application.RepoRecord)
	if !ok {
		t.Fatalf("result[\"repo\"] is not RepoRecord, got %T", m["repo"])
	}
	if repo.RepoID != "repo-2" {
		t.Errorf("expected repo-2, got %q", repo.RepoID)
	}
}

func TestAdminTools_GetRepo_NotFound(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	_, rpcErr := dispatchAdmin(t, r, "eng_get_repo", map[string]string{
		"repo_id": "no-such-repo",
	})

	if rpcErr == nil {
		t.Fatal("expected RPC error, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected -32602, got %d", rpcErr.Code)
	}
}

func TestAdminTools_GetStatus(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_get_status", map[string]any{})

	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any, got %T", result)
	}

	if m["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", m["status"])
	}

	if m["schema_version"] != 1 {
		t.Errorf("expected schema_version=1, got %v", m["schema_version"])
	}
}

func TestAdminTools_GetConfig(t *testing.T) {
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: sampleRepos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_get_config", map[string]any{})

	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any, got %T", result)
	}

	if _, hasHome := m["veska_home"]; !hasHome {
		t.Error("expected veska_home key in config response")
	}

	if m["schema_version"] != 1 {
		t.Errorf("expected schema_version=1, got %v", m["schema_version"])
	}
}
