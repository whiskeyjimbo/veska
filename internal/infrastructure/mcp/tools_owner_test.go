package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

// makeGitRepoWithCommit initialises a git repo in dir, creates file, and commits it.
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

// ---------------------------------------------------------------------------
// eng_find_owner — CODEOWNERS
// ---------------------------------------------------------------------------

func TestFindOwner_CodeownersMatch(t *testing.T) {
	dir := t.TempDir()
	// Write CODEOWNERS at repo root.
	codeowners := "*.go @go-team\n/internal/ @infra-team\n"
	if err := os.WriteFile(filepath.Join(dir, "CODEOWNERS"), []byte(codeowners), 0o644); err != nil {
		t.Fatalf("write CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "cmd/main.go",
		"repo_id":   dir,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, _ := json.Marshal(result)
	var m map[string]any
	json.Unmarshal(raw, &m)

	if m["owner"] != "@go-team" {
		t.Errorf("expected owner=@go-team, got %v", m["owner"])
	}
	if m["source"] != "codeowners" {
		t.Errorf("expected source=codeowners, got %v", m["source"])
	}
}

func TestFindOwner_CodeownersInDotGithub(t *testing.T) {
	dir := t.TempDir()
	// Write CODEOWNERS in .github/
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatalf("mkdirall .github: %v", err)
	}
	codeowners := "* @everyone\n"
	if err := os.WriteFile(filepath.Join(dir, ".github", "CODEOWNERS"), []byte(codeowners), 0o644); err != nil {
		t.Fatalf("write .github/CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "any/file.go",
		"repo_id":   dir,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, _ := json.Marshal(result)
	var m map[string]any
	json.Unmarshal(raw, &m)

	if m["owner"] != "@everyone" {
		t.Errorf("expected owner=@everyone, got %v", m["owner"])
	}
	if m["source"] != "codeowners" {
		t.Errorf("expected source=codeowners, got %v", m["source"])
	}
}

func TestFindOwner_CodeownersLongestMatchWins(t *testing.T) {
	dir := t.TempDir()
	// More specific pattern should win over wildcard.
	codeowners := "* @fallback\n/internal/mcp/ @mcp-owners\n"
	if err := os.WriteFile(filepath.Join(dir, "CODEOWNERS"), []byte(codeowners), 0o644); err != nil {
		t.Fatalf("write CODEOWNERS: %v", err)
	}

	r := NewRegistry()
	RegisterOwnerTools(r, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "internal/mcp/server.go",
		"repo_id":   dir,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, _ := json.Marshal(result)
	var m map[string]any
	json.Unmarshal(raw, &m)

	if m["owner"] != "@mcp-owners" {
		t.Errorf("expected owner=@mcp-owners (longest match), got %v", m["owner"])
	}
}

// ---------------------------------------------------------------------------
// eng_find_owner — git blame fallback
// ---------------------------------------------------------------------------

func TestFindOwner_GitBlameFallback(t *testing.T) {
	dir := t.TempDir()
	const authorEmail = "dev@example.com"
	makeGitRepoWithCommit(t, dir, "internal/app.go", authorEmail)

	r := NewRegistry()
	RegisterOwnerTools(r, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "internal/app.go",
		"repo_id":   dir,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, _ := json.Marshal(result)
	var m map[string]any
	json.Unmarshal(raw, &m)

	if m["owner"] != authorEmail {
		t.Errorf("expected owner=%s, got %v", authorEmail, m["owner"])
	}
	if m["source"] != "git_blame" {
		t.Errorf("expected source=git_blame, got %v", m["source"])
	}
}

// ---------------------------------------------------------------------------
// eng_find_owner — both fail
// ---------------------------------------------------------------------------

func TestFindOwner_BothFail(t *testing.T) {
	dir := t.TempDir()
	// No CODEOWNERS, no git repo.

	r := NewRegistry()
	RegisterOwnerTools(r, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	result, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"file_path": "some/file.go",
		"repo_id":   dir,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %v", rpcErr.Message)
	}

	raw, _ := json.Marshal(result)
	var m map[string]any
	json.Unmarshal(raw, &m)

	if m["owner"] != nil {
		t.Errorf("expected owner=nil, got %v", m["owner"])
	}
	if m["source"] != nil {
		t.Errorf("expected source=nil, got %v", m["source"])
	}
}

func TestFindOwner_MissingParams(t *testing.T) {
	r := NewRegistry()
	RegisterOwnerTools(r, nil)

	actor := domain.Actor{ID: "agent:bot", Kind: domain.ActorKindAgent}
	_, rpcErr := dispatchOwner(t, r, actor, map[string]any{
		"repo_id": "/some/dir",
		// missing file_path
	})
	if rpcErr == nil {
		t.Fatal("expected RPC error for missing file_path")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

// Ensure RegisterOwnerTools accepts a nil db (db is unused, but consistent signature).
func TestFindOwner_NilDBIsAccepted(t *testing.T) {
	var db *sql.DB // nil is fine
	r := NewRegistry()
	RegisterOwnerTools(r, db)
	if len(r.Names()) != 1 {
		t.Errorf("expected 1 tool registered, got %d", len(r.Names()))
	}
}
