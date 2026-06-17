// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type fakeRepoLister struct{ recs []application.RepoRecord }

func (f *fakeRepoLister) ListRepos(context.Context) ([]application.RepoRecord, error) {
	return f.recs, nil
}

type fakeGit struct {
	head    string
	files   []string
	headErr error
}

func (f *fakeGit) HEAD(string) (string, error)                             { return f.head, f.headErr }
func (f *fakeGit) IsAncestor(string, string, string) (bool, error)         { return false, nil }
func (f *fakeGit) CommitsSince(string, string, string) ([]string, error)   { return nil, nil }
func (f *fakeGit) ChangedFiles(string, string) ([]string, error)           { return f.files, nil }
func (f *fakeGit) ReadFileAtCommit(string, string, string) ([]byte, error) { return nil, nil }

type savedCall struct {
	RepoID, Branch, Path string
	Bytes                int
}
type fakeIng struct{ saves []savedCall }

func (f *fakeIng) Save(_ context.Context, repoID, branch, path string, src []byte) {
	f.saves = append(f.saves, savedCall{repoID, branch, path, len(src)})
}

type promoCall struct {
	RepoID, Branch, SHA string
	ActorKind           domain.ActorKind
}
type fakeProm struct {
	calls []promoCall
	err   error
}

func (f *fakeProm) Promote(_ context.Context, repoID, branch, sha string, actor domain.Actor) error {
	f.calls = append(f.calls, promoCall{repoID, branch, sha, actor.Kind})
	return f.err
}

// TestPromoteHandler_HappyPath verifies that when a registered repository has modifications at HEAD,
// the modified files are in-memory saved and a single promotion is triggered under the system actor.
func TestPromoteHandler_HappyPath(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.go", "package a\nfunc A() {}\n")
	mustWrite(t, root, "b.go", "package a\nfunc B() {}\n")

	ing := &fakeIng{}
	prom := &fakeProm{}
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{head: "sha-xyz", files: []string{"a.go", "b.go"}},
		Ingester: ing,
		Promoter: prom,
	}

	res := dispatchPromote(t, deps, map[string]string{"root_path": root})
	if res.RepoID != "r1" || res.Branch != "main" || res.GitSHA != "sha-xyz" {
		t.Errorf("result mismatch: %+v", res)
	}
	if res.FilesPromoted != 2 {
		t.Errorf("FilesPromoted = %d, want 2", res.FilesPromoted)
	}
	if len(ing.saves) != 2 {
		t.Fatalf("ingester.Save calls = %d, want 2 (%+v)", len(ing.saves), ing.saves)
	}
	for _, s := range ing.saves {
		if s.RepoID != "r1" || s.Branch != "main" || s.Bytes == 0 {
			t.Errorf("unexpected save: %+v", s)
		}
	}
	if len(prom.calls) != 1 {
		t.Fatalf("promoter.Promote calls = %d, want 1", len(prom.calls))
	}
	if c := prom.calls[0]; c.RepoID != "r1" || c.Branch != "main" || c.SHA != "sha-xyz" || c.ActorKind != domain.ActorKindSystem {
		t.Errorf("promote call mismatch: %+v", c)
	}
}

// TestPromoteHandler_RepoNotRegistered ensures that an unregistered root_path causes a CodeNotFound error instead of a silent no-op.
func TestPromoteHandler_RepoNotRegistered(t *testing.T) {
	deps := PromoteDeps{
		Repos:    &fakeRepoLister{recs: nil},
		Git:      &fakeGit{},
		Ingester: &fakeIng{},
		Promoter: &fakeProm{},
	}
	_, rpcErr := dispatchPromoteRaw(t, deps, map[string]string{"root_path": "/nowhere"})
	if rpcErr == nil {
		t.Fatal("expected error for unregistered root_path; got nil")
		return
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("error code = %d, want CodeNotFound (%d)", rpcErr.Code, CodeNotFound)
	}
	if !strings.Contains(rpcErr.Message, "not registered") {
		t.Errorf("message = %q, want 'not registered'", rpcErr.Message)
	}
}

// TestPromoteHandler_AcceptsRepoID verifies that eng_promote_repo accepts both full and short
// repository IDs as an alternative to root_path.
func TestPromoteHandler_AcceptsRepoID(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.go", "package a\n")
	ing := &fakeIng{}
	prom := &fakeProm{}
	const fullID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: fullID, RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{head: "sha-rid", files: []string{"a.go"}},
		Ingester: ing,
		Promoter: prom,
	}

	res := dispatchPromote(t, deps, map[string]string{"repo_id": fullID[:12]})
	if res.RepoID != fullID || res.GitSHA != "sha-rid" {
		t.Errorf("short-prefix dispatch: result mismatch: %+v", res)
	}

	res = dispatchPromote(t, deps, map[string]string{"repo_id": fullID})
	if res.RepoID != fullID {
		t.Errorf("full-id dispatch: result mismatch: %+v", res)
	}
}

// TestPromoteHandler_UnknownRepoID ensures that an unknown repo_id results in a CodeNotFound error.
func TestPromoteHandler_UnknownRepoID(t *testing.T) {
	deps := PromoteDeps{
		Repos:    &fakeRepoLister{recs: nil},
		Git:      &fakeGit{},
		Ingester: &fakeIng{},
		Promoter: &fakeProm{},
	}
	_, rpcErr := dispatchPromoteRaw(t, deps, map[string]string{"repo_id": "deadbeef0000"})
	if rpcErr == nil {
		t.Fatal("expected error for unknown repo_id")
		return
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("error code = %d, want CodeNotFound (%d)", rpcErr.Code, CodeNotFound)
	}
}

// TestPromoteHandler_NilDeps ensures that misconfigured dependencies result in an internal error instead of a panic.
func TestPromoteHandler_NilDeps(t *testing.T) {
	for name, deps := range map[string]PromoteDeps{
		"no repos":    {Git: &fakeGit{}, Ingester: &fakeIng{}, Promoter: &fakeProm{}},
		"no git":      {Repos: &fakeRepoLister{}, Ingester: &fakeIng{}, Promoter: &fakeProm{}},
		"no ingester": {Repos: &fakeRepoLister{}, Git: &fakeGit{}, Promoter: &fakeProm{}},
		"no promoter": {Repos: &fakeRepoLister{}, Git: &fakeGit{}, Ingester: &fakeIng{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, rpcErr := dispatchPromoteRaw(t, deps, map[string]string{"root_path": "/x"})
			if rpcErr == nil || !strings.Contains(rpcErr.Message, "not fully wired") {
				t.Errorf("want 'not fully wired' error, got %+v", rpcErr)
			}
		})
	}
}

// TestPromoteHandler_PropagatesPromoteError verifies that Promoter failures are wrapped and surfaced as RPC errors for visibility.
func TestPromoteHandler_PropagatesPromoteError(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.go", "package a\n")
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{head: "h", files: []string{"a.go"}},
		Ingester: &fakeIng{},
		Promoter: &fakeProm{err: errors.New("boom")},
	}
	_, rpcErr := dispatchPromoteRaw(t, deps, map[string]string{"root_path": root})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "boom") {
		t.Errorf("want wrapped 'boom' error, got %+v", rpcErr)
	}
}

// TestPromoteRepoSchema_PublishesAttributionParams ensures the schema defines and enforces attribution parameters while rejecting unknown fields.
func TestPromoteRepoSchema_PublishesAttributionParams(t *testing.T) {
	var s struct {
		AdditionalProperties any                        `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(promoteRepoInputSchema, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if ap, ok := s.AdditionalProperties.(bool); !ok || ap {
		t.Errorf("additionalProperties = %v, want literal false", s.AdditionalProperties)
	}
	for _, k := range []string{"repo_id", "root_path", "branch", "git_sha", "actor_kind", "actor_id"} {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("schema.properties missing %q", k)
		}
	}
	var ak struct {
		Type string   `json:"type"`
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(s.Properties["actor_kind"], &ak); err != nil {
		t.Fatalf("actor_kind sub-schema invalid: %v", err)
	}
	if ak.Type != "string" {
		t.Errorf("actor_kind.type = %q, want \"string\"", ak.Type)
	}
	want := map[string]bool{"human": true, "agent": true, "system": true}
	if len(ak.Enum) != len(want) {
		t.Errorf("actor_kind.enum = %v, want exactly %v", ak.Enum, want)
	}
	for _, v := range ak.Enum {
		if !want[v] {
			t.Errorf("actor_kind.enum contains unexpected %q", v)
		}
	}
}

// TestPromoteRepoSchema_RejectsUnknownKeyAtDispatch verifies that the dispatch validator rejects request parameters not matching the schema.
func TestPromoteRepoSchema_RejectsUnknownKeyAtDispatch(t *testing.T) {
	rpcErr := validateAgainstSchema("eng_promote_repo", promoteRepoInputSchema,
		json.RawMessage(`{"repo_id":"x","totally_made_up":"y"}`))
	if rpcErr == nil {
		t.Fatal("expected unknown-key rejection, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want CodeInvalidParams (%d)", rpcErr.Code, CodeInvalidParams)
	}
	if !strings.Contains(rpcErr.Message, "totally_made_up") {
		t.Errorf("message = %q, want it to name the offending key", rpcErr.Message)
	}
}

// TestPromoteHandler_HonoursActorOverride ensures that custom actor attribution parameters from the schema are respected during promotion.
func TestPromoteHandler_HonoursActorOverride(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.go", "package a\n")
	prom := &fakeProm{}
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{head: "head-sha", files: []string{"a.go"}},
		Ingester: &fakeIng{},
		Promoter: prom,
	}
	_ = dispatchPromote(t, deps, map[string]string{
		"root_path":  root,
		"actor_kind": "agent",
		"actor_id":   "agent:claude",
	})
	if len(prom.calls) != 1 {
		t.Fatalf("Promote calls = %d, want 1", len(prom.calls))
	}
	if prom.calls[0].ActorKind != domain.ActorKindAgent {
		t.Errorf("ActorKind = %q, want %q", prom.calls[0].ActorKind, domain.ActorKindAgent)
	}
}

// TestPromoteHandler_HonoursBranchAndSHAOverride verifies that branch and git_sha overrides
// skip the default git.HEAD lookup path.
func TestPromoteHandler_HonoursBranchAndSHAOverride(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "a.go", "package a\n")
	prom := &fakeProm{}
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{headErr: errors.New("git.HEAD must not be called when git_sha is supplied"), files: []string{"a.go"}},
		Ingester: &fakeIng{},
		Promoter: prom,
	}
	res := dispatchPromote(t, deps, map[string]string{
		"root_path": root,
		"branch":    "feature/x",
		"git_sha":   "pinned-sha",
	})
	if res.Branch != "feature/x" || res.GitSHA != "pinned-sha" {
		t.Errorf("result = %+v; want branch=feature/x git_sha=pinned-sha", res)
	}
	if len(prom.calls) != 1 || prom.calls[0].Branch != "feature/x" || prom.calls[0].SHA != "pinned-sha" {
		t.Errorf("Promote called with %+v; want branch=feature/x sha=pinned-sha", prom.calls)
	}
}

// TestPromoteHandler_RejectsInvalidActorKind ensures that custom clients bypassing the schema are still rejected at the handler if the actor kind is invalid.
func TestPromoteHandler_RejectsInvalidActorKind(t *testing.T) {
	root := t.TempDir()
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{head: "h", files: nil},
		Ingester: &fakeIng{},
		Promoter: &fakeProm{},
	}
	_, rpcErr := dispatchPromoteRaw(t, deps, map[string]string{
		"root_path":  root,
		"actor_kind": "robot",
		"actor_id":   "robot:1",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams for actor_kind=robot, got %+v", rpcErr)
	}
}

// TestPromoteHandler_RejectsPartialActor ensures that a request providing only one of the actor_kind or actor_id parameters is rejected.
func TestPromoteHandler_RejectsPartialActor(t *testing.T) {
	root := t.TempDir()
	deps := PromoteDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: realPath(t, root), ActiveBranch: "main"},
		}},
		Git:      &fakeGit{head: "h"},
		Ingester: &fakeIng{},
		Promoter: &fakeProm{},
	}
	_, rpcErr := dispatchPromoteRaw(t, deps, map[string]string{
		"root_path":  root,
		"actor_kind": "agent",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams for partial actor, got %+v", rpcErr)
	}
}

func mustWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// realPath resolves symlinks in the path to match the canonicalization behavior of the promotion handler.
func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

func dispatchPromote(t *testing.T, deps PromoteDeps, params any) promoteResult {
	t.Helper()
	raw, _ := json.Marshal(params)
	out, rpcErr := makePromoteHandler(deps)(context.Background(),
		domain.Actor{ID: "human:test", Kind: domain.ActorKindHuman}, raw)
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	r, ok := out.(promoteResult)
	if !ok {
		t.Fatalf("result type = %T, want promoteResult", out)
	}
	return r
}

func dispatchPromoteRaw(t *testing.T, deps PromoteDeps, params any) (any, *RPCError) {
	t.Helper()
	raw, _ := json.Marshal(params)
	return makePromoteHandler(deps)(context.Background(),
		domain.Actor{ID: "human:test", Kind: domain.ActorKindHuman}, raw)
}
