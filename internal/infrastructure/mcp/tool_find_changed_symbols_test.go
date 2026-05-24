package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

type csMemFiles map[string]string

func (m csMemFiles) changedFiles(_ context.Context, _, _, _ string) ([]string, error) {
	return []string{"code.go"}, nil
}

func (m csMemFiles) fileAtRef(_ context.Context, _, ref, path string) ([]byte, error) {
	if c, ok := m[ref+":"+path]; ok {
		return []byte(c), nil
	}
	return nil, errors.New("not present at ref")
}

func newChangedSymbolsRegistry(t *testing.T, m csMemFiles) *Registry {
	t.Helper()
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), m.changedFiles, m.fileAtRef)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r := NewRegistry()
	RegisterChangedSymbolsTool(r, svc, func(context.Context, string) (string, error) {
		return "/root", nil
	}, nil)
	return r
}

func dispatchChangedSymbols(t *testing.T, r *Registry, params any) (changedsymbols.Result, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{Method: "eng_find_changed_symbols", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(),
		domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return changedsymbols.Result{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var resp changedsymbols.Result
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	return resp, nil
}

func TestChangedSymbols_ReturnsThreeBuckets(t *testing.T) {
	m := csMemFiles{
		"refA:code.go": "package p\nfunc Keep() {}\nfunc Gone() {}\nfunc Edit() {}\n",
		"refB:code.go": "package p\nfunc Keep() {}\nfunc Edit() { _ = 1 }\nfunc Fresh() {}\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main", "ref_a": "refA", "ref_b": "refB",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr)
	}
	if len(resp.Added) != 1 || resp.Added[0].Name != "Fresh" {
		t.Errorf("added = %+v, want [Fresh]", resp.Added)
	}
	if len(resp.Removed) != 1 || resp.Removed[0].Name != "Gone" {
		t.Errorf("removed = %+v, want [Gone]", resp.Removed)
	}
	if len(resp.Modified) != 1 || resp.Modified[0].Name != "Edit" {
		t.Errorf("modified = %+v, want [Edit]", resp.Modified)
	}
}

// TestChangedSymbols_EmptyBucketsSerializeAsArrays guards solov2-jbgt: empty
// added/removed/modified must JSON-render as [] (not null) to match the MCP
// surface contract.
func TestChangedSymbols_EmptyBucketsSerializeAsArrays(t *testing.T) {
	m := csMemFiles{
		"refA:code.go": "package p\nfunc Same() {}\n",
		"refB:code.go": "package p\nfunc Same() {}\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	raw, _ := json.Marshal(map[string]string{
		"repo_id": "repo1", "branch": "main", "ref_a": "refA", "ref_b": "refB",
	})
	req := &Request{Method: "eng_find_changed_symbols", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(),
		domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr)
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, field := range []string{`"added":[]`, `"removed":[]`, `"modified":[]`} {
		if !contains(got, field) {
			t.Errorf("expected %s in JSON, got: %s", field, got)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestChangedSymbols_DefaultsToLastCommit guards solov2-npjs: omitting both
// refs must default to HEAD~1..HEAD rather than erroring on missing params.
func TestChangedSymbols_DefaultsToLastCommit(t *testing.T) {
	m := csMemFiles{
		"HEAD~1:code.go": "package p\nfunc Old() {}\n",
		"HEAD:code.go":   "package p\nfunc Old() {}\nfunc New() {}\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("default refs rejected: %+v", rpcErr)
	}
	if len(resp.Added) != 1 || resp.Added[0].Name != "New" {
		t.Errorf("added = %+v, want [New] from HEAD~1..HEAD default", resp.Added)
	}
}

func TestChangedSymbols_RequiredParams(t *testing.T) {
	m := csMemFiles{}
	r := newChangedSymbolsRegistry(t, m)
	cases := []map[string]string{
		{"branch": "main", "ref_a": "a", "ref_b": "b"},   // no repo_id
		{"repo_id": "r", "ref_a": "a", "ref_b": "b"},     // no branch
		{"repo_id": "r", "branch": "main", "ref_b": "b"}, // no ref_a
		{"repo_id": "r", "branch": "main", "ref_a": "a"}, // no ref_b
	}
	for i, c := range cases {
		_, rpcErr := dispatchChangedSymbols(t, r, c)
		if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
			t.Errorf("case %d: expected InvalidParams, got %v", i, rpcErr)
		}
	}
}

func TestChangedSymbols_NotWiredReturnsInternalError(t *testing.T) {
	r := NewRegistry()
	RegisterChangedSymbolsTool(r, nil, nil, nil)
	_, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "r", "branch": "main", "ref_a": "a", "ref_b": "b",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Errorf("expected InternalError, got %v", rpcErr)
	}
}
