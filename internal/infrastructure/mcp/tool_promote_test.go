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

// ---- fakes -----------------------------------------------------------------

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

// ---- tests -----------------------------------------------------------------

// TestPromoteHandler_HappyPath: a registered repo's HEAD-changed files are
// re-Saved and Promote is called once at HEAD with the system actor. This
// is the end-to-end shape the post-commit hook depends on (solov2-3vv).
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

// TestPromoteHandler_RepoNotRegistered: an unknown root_path returns
// InvalidParams rather than silently no-op'ing. The previous {"cmd":"promote"}
// protocol was a silent black hole; we want this one to be loud.
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
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want CodeInvalidParams (%d)", rpcErr.Code, CodeInvalidParams)
	}
	if !strings.Contains(rpcErr.Message, "not registered") {
		t.Errorf("message = %q, want 'not registered'", rpcErr.Message)
	}
}

// TestPromoteHandler_NilDeps: misconfigured wiring returns an internal-error
// rather than panicking. Surface-level safety check.
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

// TestPromoteHandler_PropagatesPromoteError: a Promoter failure surfaces as
// an internal-error so the daemon's hook-runner log can show what broke.
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

// ---- helpers ----------------------------------------------------------------

func mustWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// realPath returns the EvalSymlinks form of p — matches what the handler
// canonicalises root_path to before lookup.
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
