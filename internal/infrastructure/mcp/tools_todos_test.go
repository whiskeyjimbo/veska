// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

type stubTodoQuerier struct {
	entries []ports.TodoEntry
	err     error

	gotOnlyOpen bool
}

func (s *stubTodoQuerier) FindTodos(_ context.Context, _, _ string, onlyOpen bool) ([]ports.TodoEntry, error) {
	s.gotOnlyOpen = onlyOpen
	if s.err != nil {
		return nil, s.err
	}
	return s.entries, nil
}

func dispatchTodos(t *testing.T, r *Registry, params any) (TodosResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: "eng_find_todos", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return TodosResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp TodosResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

func TestFindTodos_ReturnsOpenByDefault(t *testing.T) {
	q := &stubTodoQuerier{entries: []ports.TodoEntry{
		{FindingID: "t1", RepoID: "r", Branch: "main", FilePath: "a.go", Message: "TODO: x", State: "open"},
	}}
	r := NewRegistry()
	RegisterTodoTools(r, q, nil)

	resp, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r", "branch": "main"})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if !q.gotOnlyOpen {
		t.Error("expected onlyOpen=true by default")
	}
	if len(resp.Todos) != 1 || resp.Todos[0].FindingID != "t1" {
		t.Errorf("unexpected todos: %+v", resp.Todos)
	}
}

// TestFindTodos_EmitsSnakeCaseKeys verifies that the serialized JSON keys are snake_case rather than PascalCase struct field names.
func TestFindTodos_EmitsSnakeCaseKeys(t *testing.T) {
	q := &stubTodoQuerier{entries: []ports.TodoEntry{
		{FindingID: "t1", RepoID: "r", Branch: "main", FilePath: "a.go", Message: "TODO: x", State: "open", CreatedAt: 42},
	}}
	r := NewRegistry()
	RegisterTodoTools(r, q, nil)

	raw, _ := json.Marshal(map[string]string{"repo_id": "r", "branch": "main"})
	req := &Request{Method: "eng_find_todos", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	js := string(b)
	for _, want := range []string{"finding_id", "repo_id", "file_path", "created_at"} {
		if !strings.Contains(js, want) {
			t.Errorf("response missing snake_case key %q: %s", want, js)
		}
	}
	for _, bad := range []string{"FindingID", "RepoID", "FilePath", "CreatedAt"} {
		if strings.Contains(js, bad) {
			t.Errorf("response leaked PascalCase key %q: %s", bad, js)
		}
	}
}

// TestFindTodos_EmitsDegradedReasonsAsEmptyArray ensures degraded_reasons is always serialized as an array, even when empty.
func TestFindTodos_EmitsDegradedReasonsAsEmptyArray(t *testing.T) {
	q := &stubTodoQuerier{entries: nil}
	r := NewRegistry()
	RegisterTodoTools(r, q, nil)

	raw, _ := json.Marshal(map[string]string{"repo_id": "r", "branch": "main"})
	req := &Request{Method: "eng_find_todos", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	b, _ := json.Marshal(result)
	js := string(b)
	if !strings.Contains(js, `"degraded_reasons":[]`) {
		t.Errorf("expected degraded_reasons:[] in JSON, got: %s", js)
	}
}

func TestFindTodos_IncludeClosedFlipsOnlyOpen(t *testing.T) {
	q := &stubTodoQuerier{}
	r := NewRegistry()
	RegisterTodoTools(r, q, nil)

	_, rpcErr := dispatchTodos(t, r, map[string]any{
		"repo_id": "r", "branch": "main", "include_closed": true,
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if q.gotOnlyOpen {
		t.Error("expected onlyOpen=false when include_closed=true")
	}
}

func TestFindTodos_RequiresParams(t *testing.T) {
	r := NewRegistry()
	RegisterTodoTools(r, &stubTodoQuerier{}, nil)

	_, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r"})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestFindTodos_PropagatesError(t *testing.T) {
	q := &stubTodoQuerier{err: errors.New("disk full")}
	r := NewRegistry()
	RegisterTodoTools(r, q, nil)

	_, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}

func TestFindTodos_RegistersOneTool(t *testing.T) {
	r := NewRegistry()
	RegisterTodoTools(r, &stubTodoQuerier{}, nil)
	got := r.Names()
	if len(got) != 1 || got[0] != "eng_find_todos" {
		t.Fatalf("expected [eng_find_todos], got %v", got)
	}
}

// TestFindTodos_RelativizesAbsolutePath ensures absolute paths in todo entries are relative to the repository root when the repository is listed.
func TestFindTodos_RelativizesAbsolutePath(t *testing.T) {
	q := &stubTodoQuerier{entries: []ports.TodoEntry{
		{
			FindingID: "t1", RepoID: "r", Branch: "main",
			FilePath: "/abs/repo/internal/server/server.go",
			Message:  "TODO: x", State: "open",
		},
	}}
	r := NewRegistry()
	repos := &fakeRepoLister{recs: []application.RepoRecord{
		{RepoID: "r", RootPath: "/abs/repo", ActiveBranch: "main"},
	}}
	RegisterTodoTools(r, q, repos)

	resp, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r"})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Todos) != 1 {
		t.Fatalf("expected 1 todo, got %d", len(resp.Todos))
	}
	want := "internal/server/server.go"
	if got := resp.Todos[0].FilePath; got != want {
		t.Errorf("file_path = %q, want %q (repo-relative per solov2-62gc/v7dq)", got, want)
	}
}

// TestFindTodos_DegradedWhenWorkingTreeDirty ensures a dirty working tree returns the todos_are_post_promotion degraded reason.
func TestFindTodos_DegradedWhenWorkingTreeDirty(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		runCmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := runCmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "j@e")
	runGit("config", "user.name", "j")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("add", "a.go")
	runGit("commit", "--no-gpg-sign", "-m", "init")

	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n// TODO: pending\n"), 0o644); err != nil {
		t.Fatalf("write2: %v", err)
	}

	q := &stubTodoQuerier{entries: nil}
	r := NewRegistry()
	repos := &fakeRepoLister{recs: []application.RepoRecord{{RepoID: "r", RootPath: dir, ActiveBranch: "main"}}}
	RegisterTodoTools(r, q, repos)

	resp, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r"})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Todos) != 0 {
		t.Fatalf("expected 0 todos, got %d", len(resp.Todos))
	}
	found := false
	for _, d := range resp.DegradedReasons {
		if d == "todos_are_post_promotion" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected todos_are_post_promotion in degraded_reasons; got %v", resp.DegradedReasons)
	}
}

// TestFindTodos_CleanRepoStaysUndegraded ensures that a clean working tree does not carry the todos_are_post_promotion degraded reason.
func TestFindTodos_CleanRepoStaysUndegraded(t *testing.T) {
	dir := t.TempDir()
	runCmd := exec.Command("git", "-C", dir, "init")
	if out, err := runCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	q := &stubTodoQuerier{entries: nil}
	r := NewRegistry()
	repos := &fakeRepoLister{recs: []application.RepoRecord{{RepoID: "r", RootPath: dir, ActiveBranch: "main"}}}
	RegisterTodoTools(r, q, repos)

	resp, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r"})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	for _, d := range resp.DegradedReasons {
		if d == "todos_are_post_promotion" {
			t.Errorf("clean tree should not carry todos_are_post_promotion; got %v", resp.DegradedReasons)
		}
	}
}

func TestFindTodos_LeavesAlreadyRelativePath(t *testing.T) {
	q := &stubTodoQuerier{entries: []ports.TodoEntry{
		{FindingID: "t1", RepoID: "r", Branch: "main", FilePath: "pkg/x.go", State: "open"},
	}}
	r := NewRegistry()
	repos := &fakeRepoLister{recs: []application.RepoRecord{
		{RepoID: "r", RootPath: "/abs/repo", ActiveBranch: "main"},
	}}
	RegisterTodoTools(r, q, repos)

	resp, rpcErr := dispatchTodos(t, r, map[string]string{"repo_id": "r"})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if got := resp.Todos[0].FilePath; got != "pkg/x.go" {
		t.Errorf("file_path = %q, want pkg/x.go (already relative)", got)
	}
}
