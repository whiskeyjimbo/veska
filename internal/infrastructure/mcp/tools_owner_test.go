package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)


func dispatchOwner(t *testing.T, r *Registry, actor domain.Actor, params map[string]any) (any, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{
		JSONRPC: "2.0",
		Method:  "eng_find_owner",
		Params:  raw,
	}
	return r.Dispatch(context.Background(), actor, req)
}


func makeGitRepoWithCommit(t *testing.T, dir, filePath, authorEmail string) {
	t.Helper()
	cmds := [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", authorEmail},
		{"git", "-C", dir, "config", "user.name", "Test Author"},
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("cmd %v: %v\n%s", c, err, out)
		}
	}

	fullPath := filepath.Join(dir, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	addCommit := [][]string{
		{"git", "-C", dir, "add", filePath},
		{"git", "-C", dir, "commit", "--no-gpg-sign", "-m", "initial"},
	}
	for _, c := range addCommit {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("cmd %v: %v\n%s", c, err, out)
		}
	}
}

// TestFindOwner_AcceptsBranchParam verifies that the tool schema accepts "branch"
// alongside repo_id to ensure consistency with other read-side eng_* tools.
func TestFindOwner_AcceptsBranchParam(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CODEOWNERS"), []byte("*.go @go-team\n"), 0o644); err != nil {
		t.Fatalf("write CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "cmd/main.go",
		"repo_id":   dir,
		"branch":    "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error with branch param: %+v", rpcErr)
	}
}

func TestFindOwner_CodeownersMatch(t *testing.T) {
	dir := t.TempDir()
	codeowners := "*.go @go-team\n/internal/ @infra-team\n"
	if err := os.WriteFile(filepath.Join(dir, "CODEOWNERS"), []byte(codeowners), 0o644); err != nil {
		t.Fatalf("write CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "cmd/main.go",
		"repo_id":   dir,
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

	if m["owner"] != "@go-team" {
		t.Errorf("expected owner=@go-team, got %v", m["owner"])
	}
	if m["source"] != "codeowners" {
		t.Errorf("expected source=codeowners, got %v", m["source"])
	}
}

func TestFindOwner_CodeownersInDotGithub(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatalf("mkdirall .github: %v", err)
	}
	codeowners := "* @everyone\n"
	if err := os.WriteFile(filepath.Join(dir, ".github", "CODEOWNERS"), []byte(codeowners), 0o644); err != nil {
		t.Fatalf("write .github/CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "any/file.go",
		"repo_id":   dir,
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

	if m["owner"] != "@everyone" {
		t.Errorf("expected owner=@everyone, got %v", m["owner"])
	}
	if m["source"] != "codeowners" {
		t.Errorf("expected source=codeowners, got %v", m["source"])
	}
}

func TestFindOwner_CodeownersLongestMatchWins(t *testing.T) {
	dir := t.TempDir()
	codeowners := "* @fallback\n/internal/mcp/ @mcp-owners\n"
	if err := os.WriteFile(filepath.Join(dir, "CODEOWNERS"), []byte(codeowners), 0o644); err != nil {
		t.Fatalf("write CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "internal/mcp/server.go",
		"repo_id":   dir,
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

	if m["owner"] != "@mcp-owners" {
		t.Errorf("expected owner=@mcp-owners (longest match), got %v", m["owner"])
	}
}

func TestFindOwner_GitBlameFallback(t *testing.T) {
	dir := t.TempDir()
	const authorEmail = "dev@example.com"
	makeGitRepoWithCommit(t, dir, "internal/app.go", authorEmail)

	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "internal/app.go",
		"repo_id":   dir,
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

	if m["owner"] != authorEmail {
		t.Errorf("expected owner=%s, got %v", authorEmail, m["owner"])
	}
	if m["source"] != "git_blame" {
		t.Errorf("expected source=git_blame, got %v", m["source"])
	}
}

func TestFindOwner_BothFail(t *testing.T) {
	dir := t.TempDir()

	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "some/file.go",
		"repo_id":   dir,
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

	if m["owner"] != nil {
		t.Errorf("expected owner=nil, got %v", m["owner"])
	}
	if m["source"] != nil {
		t.Errorf("expected source=nil, got %v", m["source"])
	}
	// A null owner path must include a diagnostics reason to help distinguish
	// a missing CODEOWNERS file from a file that exists but lacks a matching pattern.
	reason, _ := m["reason"].(string)
	if reason == "" {
		t.Errorf("expected non-empty reason on null path, got %q", reason)
	}
	if !strings.Contains(reason, "CODEOWNERS") {
		t.Errorf("reason should mention CODEOWNERS for diagnosability, got: %q", reason)
	}
}

func TestFindOwner_MissingParams(t *testing.T) {
	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"repo_id": "/some/dir",
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing file_path")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

// TestFindOwner_AcceptsPathAlias verifies that the schema accepts "path" as an alias
// for "file_path" and delegates gracefully instead of rejecting the parameter as unknown.
func TestFindOwner_AcceptsPathAlias(t *testing.T) {
	repoRoot, _ := os.Getwd()
	r := NewRegistry()
	RegisterOwnerTools(r, nil, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	res, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"repo_id": repoRoot,
		"path":    "some/file.go",
	})
	if rpcErr != nil {
		t.Fatalf("path alias rejected: %+v", rpcErr)
	}
	// The result may indicate a null owner if the file does not exist, but the
	// response envelope structure must still be returned instead of an RPC error.
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map response, got %T: %+v", res, res)
	}
	if _, ok := m["owner"]; !ok {
		if _, hasReason := m["reason"]; !hasReason {
			t.Errorf("expected owner or reason in response, got %+v", m)
		}
	}
}

// TestFindOwner_NilDBIsAccepted ensures RegisterOwnerTools accepts a nil db
// for API compatibility, even though db is unused during the lookup.
func TestFindOwner_NilDBIsAccepted(t *testing.T) {
	var db *sql.DB
	r := NewRegistry()
	RegisterOwnerTools(r, db, nil)
	if len(r.Names()) != 1 {
		t.Errorf("expected 1 tool registered, got %d", len(r.Names()))
	}
}

// TestFindOwner_MissingRepoIDHintMatchesPeers ensures the error message guides
// the user to list repositories when repo_id is missing and cannot be inferred.
func TestFindOwner_MissingRepoIDHintMatchesPeers(t *testing.T) {
	r := NewRegistry()
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "r1", RootPath: "/r1", ActiveBranch: "main"},
		{RepoID: "r2", RootPath: "/r2", ActiveBranch: "main"},
	}}
	RegisterOwnerTools(r, nil, repos)

	_, rpcErr := dispatchOwner(t, r, domain.Actor{}, map[string]any{
		"file_path": "x.go",
	})
	if rpcErr == nil {
		t.Fatal("want RPC error, got nil")
	}
	if !strings.Contains(rpcErr.Message, "2 repos registered") ||
		!strings.Contains(rpcErr.Message, "eng_list_repos") {
		t.Errorf("missing peer-style hint, got %q", rpcErr.Message)
	}
}
