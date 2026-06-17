package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type stubRepoLister struct {
	repos []application.RepoRecord
	err   error
}

func (s *stubRepoLister) ListRepos(_ context.Context) ([]application.RepoRecord, error) {
	return s.repos, s.err
}

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
	return r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
}

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

	repo, ok := m["repo"].(RepoView)
	if !ok {
		t.Fatalf("result[\"repo\"] is not RepoView, got %T", m["repo"])
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
		return
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
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected -32602, got %d", rpcErr.Code)
	}
}

// We exclude synthetic external dependency repository records from sole-visible auto-resolution when no working directory is specified.
func TestAdminTools_GetCurrentRepo_SoleVisibleIgnoresExt(t *testing.T) {
	r := NewRegistry()
	repos := []application.RepoRecord{
		{RepoID: "ext:github.com/spf13/cobra", RootPath: "/vendor/cobra", ActiveBranch: "main"},
		{RepoID: "repo-real", RootPath: "/home/user/project", ActiveBranch: "main", LastPromotedSHA: "abc"},
	}
	RegisterAdminTools(r, &stubRepoLister{repos: repos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_get_current_repo", map[string]string{})
	if rpcErr != nil {
		t.Fatalf("expected sole-visible auto-resolve, got RPC error: %+v", rpcErr)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any, got %T", result)
	}
	repo, ok := m["repo"].(RepoView)
	if !ok {
		t.Fatalf("result[\"repo\"] is not RepoView, got %T", m["repo"])
	}
	if repo.RepoID != "repo-real" {
		t.Errorf("expected sole visible repo-real, got %q", repo.RepoID)
	}
	degraded, _ := m["degraded_reasons"].([]string)
	if len(degraded) != 1 || degraded[0] != "defaulted_to_sole_repo" {
		t.Errorf("expected degraded_reasons=[defaulted_to_sole_repo], got %v", degraded)
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

	repos, ok := m["repos"].([]RepoView)
	if !ok {
		t.Fatalf("repos missing or wrong type: %T", m["repos"])
	}
	if len(repos) != 2 {
		t.Errorf("expected 2 repos, got %d", len(repos))
	}
	for _, r := range repos {
		if r.Status == "" {
			t.Errorf("repo %q missing status field", r.RepoID)
		}
	}

	degraded, ok := m["degraded_reasons"].([]string)
	if !ok {
		t.Fatalf("degraded_reasons missing or wrong type")
	}
	if len(degraded) != 0 {
		t.Errorf("expected empty degraded_reasons")
	}

	// We round-trip the serialization of RepoView to guarantee that empty slices like Aliases serialize as empty arrays rather than null.
	for _, v := range repos {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal RepoView: %v", err)
		}
		if !strings.Contains(string(b), `"aliases":[]`) {
			t.Errorf("repo %q must serialise aliases as []; got %s", v.RepoID, b)
		}
	}
}

// If a repository's root path is missing from the filesystem, we report its status as 'missing'.
func TestAdminTools_ListRepos_MissingRoot(t *testing.T) {
	live := t.TempDir()
	gone := t.TempDir() + "/never-existed"
	repos := []application.RepoRecord{
		{RepoID: "live-repo-id", RootPath: live, ActiveBranch: "main", LastPromotedSHA: "sha"},
		{RepoID: "gone-repo-id", RootPath: gone, ActiveBranch: "main", LastPromotedSHA: "sha"},
	}
	r := NewRegistry()
	RegisterAdminTools(r, &stubRepoLister{repos: repos}, nil, nil)

	result, rpcErr := dispatchAdmin(t, r, "eng_list_repos", map[string]any{})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	views := result.(map[string]any)["repos"].([]RepoView)
	byID := map[string]string{}
	for _, v := range views {
		byID[v.RepoID] = v.Status
	}
	if byID["live-repo-id"] != "promoted" {
		t.Errorf("live repo status = %q, want promoted", byID["live-repo-id"])
	}
	if byID["gone-repo-id"] != "missing" {
		t.Errorf("gone repo status = %q, want missing", byID["gone-repo-id"])
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

	repo, ok := m["repo"].(RepoView)
	if !ok {
		t.Fatalf("result[\"repo\"] is not RepoView, got %T", m["repo"])
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
		return
	}
	// A missing repository resource must return a CodeNotFound RPC error instead of CodeInvalidParams.
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected %d, got %d", CodeNotFound, rpcErr.Code)
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

	if m["config_schema_version"] != 1 {
		t.Errorf("expected config_schema_version=1, got %v", m["config_schema_version"])
	}
}
