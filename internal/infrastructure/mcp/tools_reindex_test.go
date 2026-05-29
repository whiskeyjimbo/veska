package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestReindexHandler_ResolvesByRepoID asserts that eng_reindex_repo runs the
// reparser exactly once with the resolved record when called with a short_id.
func TestReindexHandler_ResolvesByRepoID(t *testing.T) {
	const fullID = "deadbeefcafebabe0000000000000000"
	var got application.RepoRecord
	var calls atomic.Int32
	deps := ReindexDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: fullID, RootPath: "/r", ActiveBranch: "main"},
		}},
		Reparser: func(_ context.Context, rec application.RepoRecord) error {
			calls.Add(1)
			got = rec
			return nil
		},
	}
	out, rpcErr := dispatchReindex(t, deps, map[string]string{"repo_id": ShortRepoID(fullID)})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("reparser invocations = %d, want 1", calls.Load())
	}
	if got.RepoID != fullID {
		t.Errorf("got rec.RepoID = %q, want %q", got.RepoID, fullID)
	}
	r, ok := out.(reindexResult)
	if !ok {
		t.Fatalf("result type = %T, want reindexResult", out)
	}
	if r.RepoID != fullID || r.Status != "complete" {
		t.Errorf("result mismatch: %+v", r)
	}
}

func TestReindexHandler_ResolvesByRootPath(t *testing.T) {
	root := t.TempDir()
	canon := realPath(t, root)
	var calls atomic.Int32
	deps := ReindexDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: canon, ActiveBranch: "trunk"},
		}},
		Reparser: func(_ context.Context, rec application.RepoRecord) error {
			calls.Add(1)
			if rec.ActiveBranch != "trunk" {
				t.Errorf("reparser branch = %q, want %q", rec.ActiveBranch, "trunk")
			}
			return nil
		},
	}
	_, rpcErr := dispatchReindex(t, deps, map[string]string{"root_path": root})
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("reparser invocations = %d, want 1", calls.Load())
	}
}

func TestReindexHandler_RejectsMissingInputs(t *testing.T) {
	deps := ReindexDeps{
		Repos:    &fakeRepoLister{},
		Reparser: func(context.Context, application.RepoRecord) error { return nil },
	}
	_, rpcErr := dispatchReindex(t, deps, map[string]string{})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %+v", rpcErr)
	}
}

func TestReindexHandler_UnknownRepo(t *testing.T) {
	deps := ReindexDeps{
		Repos:    &fakeRepoLister{recs: nil},
		Reparser: func(context.Context, application.RepoRecord) error { return nil },
	}
	_, rpcErr := dispatchReindex(t, deps, map[string]string{"repo_id": "nosuch"})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("want CodeNotFound, got %+v", rpcErr)
	}
}

func TestReindexHandler_ReparserErrorWraps(t *testing.T) {
	deps := ReindexDeps{
		Repos: &fakeRepoLister{recs: []application.RepoRecord{
			{RepoID: "r1", RootPath: "/r"},
		}},
		Reparser: func(context.Context, application.RepoRecord) error { return errors.New("boom") },
	}
	_, rpcErr := dispatchReindex(t, deps, map[string]string{"repo_id": "r1"})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "boom") {
		t.Fatalf("want wrapped 'boom', got %+v", rpcErr)
	}
}

func TestReindexHandler_NilDepsReturnsInternalError(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps ReindexDeps
	}{
		{"nil repos", ReindexDeps{Reparser: func(context.Context, application.RepoRecord) error { return nil }}},
		{"nil reparser", ReindexDeps{Repos: &fakeRepoLister{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rpcErr := dispatchReindex(t, tc.deps, map[string]string{"repo_id": "x"})
			if rpcErr == nil || rpcErr.Code != CodeInternalError {
				t.Fatalf("want CodeInternalError, got %+v", rpcErr)
			}
		})
	}
}

// TestReindexRepoSchema_AdditionalPropertiesFalse pins the solov2-9bzq
// invariant: every new MCP tool's schema must reject unknown keys.
func TestReindexRepoSchema_AdditionalPropertiesFalse(t *testing.T) {
	var s struct {
		AdditionalProperties any                        `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(reindexRepoInputSchema, &s); err != nil {
		t.Fatalf("schema invalid: %v", err)
	}
	if ap, ok := s.AdditionalProperties.(bool); !ok || ap {
		t.Errorf("additionalProperties = %v, want literal false", s.AdditionalProperties)
	}
	for _, k := range []string{"repo_id", "root_path"} {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("schema.properties missing %q", k)
		}
	}
}

func dispatchReindex(t *testing.T, deps ReindexDeps, params any) (any, *RPCError) {
	t.Helper()
	raw, _ := json.Marshal(params)
	return makeReindexHandler(deps)(context.Background(),
		domain.Actor{ID: "human:test", Kind: domain.ActorKindHuman}, raw)
}
