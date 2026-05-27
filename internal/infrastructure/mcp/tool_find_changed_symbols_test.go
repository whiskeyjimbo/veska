package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
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
	// solov2-w8nr: file_path must be absolute, matching the contract used by
	// every other node-emitting tool. The MCP handler rewrites the service's
	// repo-relative paths against the resolved root ("/root" in this test).
	if resp.Added[0].FilePath != "/root/code.go" {
		t.Errorf("added[0].file_path = %q, want absolute /root/code.go", resp.Added[0].FilePath)
	}
	if len(resp.Removed) != 1 || resp.Removed[0].Name != "Gone" {
		t.Errorf("removed = %+v, want [Gone]", resp.Removed)
	}
	if len(resp.Modified) != 1 || resp.Modified[0].Name != "Edit" {
		t.Errorf("modified = %+v, want [Edit]", resp.Modified)
	}
}

// TestChangedSymbols_FiltersChunkEntries covers solov2-u9os: a comment- or
// whitespace-only change creates a chunk diff (KindChunk) in the parser
// output, but it isn't a symbol from the user's perspective. The
// changedsymbols service must filter chunks and surface a
// "non_symbol_changes_only" degraded reason instead so agents don't see
// "chunk:N-M" entries leaking into added/removed/modified.
func TestChangedSymbols_FiltersChunkEntries(t *testing.T) {
	// Two refs whose only difference is a comment line; symbols are
	// identical, but the parser will still emit different chunk nodes
	// because the file body changed.
	m := csMemFiles{
		"refA:code.go": "package p\n// old comment\nfunc Keep() {}\n",
		"refB:code.go": "package p\n// new comment\nfunc Keep() {}\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main", "ref_a": "refA", "ref_b": "refB",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr)
	}
	for _, bucket := range [][]changedsymbols.SymbolChange{resp.Added, resp.Removed, resp.Modified} {
		for _, c := range bucket {
			if strings.HasPrefix(c.Name, "chunk:") {
				t.Errorf("chunk leaked into response: %+v", c)
			}
		}
	}
	// At least one of the buckets must be empty or the degraded reason
	// signalling 'comments-only changes' must be set so a caller knows
	// the file did change.
	if len(resp.Added)+len(resp.Removed)+len(resp.Modified) == 0 {
		found := slices.Contains(resp.DegradedReasons, changedsymbols.DegradedReasonNonSymbolChangesOnly)
		if !found {
			t.Errorf("comment-only diff must set degraded_reasons=%q, got %+v",
				changedsymbols.DegradedReasonNonSymbolChangesOnly, resp.DegradedReasons)
		}
	}
}

// TestChangedSymbols_AcceptsBaseHeadAliases covers solov2-3ocy: git's
// canonical "base/head" param names must be accepted alongside ref_a/ref_b.
// Junior agents reach for base/head naturally — pre-fix, the schema's
// additionalProperties:false rejected them with an "unknown parameter"
// error before the handler ever ran.
func TestChangedSymbols_AcceptsBaseHeadAliases(t *testing.T) {
	m := csMemFiles{
		"refA:code.go": "package p\nfunc Keep() {}\nfunc Gone() {}\n",
		"refB:code.go": "package p\nfunc Keep() {}\nfunc Fresh() {}\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main", "base": "refA", "head": "refB",
	})
	if rpcErr != nil {
		t.Fatalf("base/head aliases rejected: %v", rpcErr)
	}
	if len(resp.Added) != 1 || resp.Added[0].Name != "Fresh" {
		t.Errorf("added = %+v, want [Fresh]", resp.Added)
	}
	if len(resp.Removed) != 1 || resp.Removed[0].Name != "Gone" {
		t.Errorf("removed = %+v, want [Gone]", resp.Removed)
	}
}

// TestChangedSymbols_RejectsConflictingAliases: when both ref_a and base
// are supplied with DIFFERENT values, surface a clear param error rather
// than picking one silently.
func TestChangedSymbols_RejectsConflictingAliases(t *testing.T) {
	m := csMemFiles{
		"refA:code.go": "package p\n",
		"refB:code.go": "package p\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	_, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main",
		"ref_a": "refA", "base": "main", "ref_b": "refB",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams for conflicting ref_a/base, got %+v", rpcErr)
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

// TestChangedSymbols_SingleCommitRepoFallsBackToEmptyTree pins solov2-wrbn:
// the default HEAD~1..HEAD pair fails on a freshly-promoted single-commit
// repo (the literal first-run journey). The handler must detect the
// unknown-revision error on the default path and retry against the
// canonical empty-tree SHA, so every symbol in HEAD comes back as
// "added" instead of the user seeing a self-contradicting "try omitting
// both refs" message.
func TestChangedSymbols_SingleCommitRepoFallsBackToEmptyTree(t *testing.T) {
	const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	// changedFiles errors when asked about HEAD~1 (single-commit case) and
	// returns the file when asked against the empty-tree retry.
	changedFiles := func(_ context.Context, _, refA, _ string) ([]string, error) {
		if refA == "HEAD~1" {
			return nil, fmt.Errorf("%w: refs=HEAD~1..HEAD", gitinfra.ErrUnknownRevision)
		}
		if refA == emptyTreeSHA {
			return []string{"code.go"}, nil
		}
		return nil, fmt.Errorf("unexpected refA %q", refA)
	}
	// fileAtRef: at HEAD the file has one symbol; at the empty tree the
	// file is absent (handled as empty by the service).
	fileAtRef := func(_ context.Context, _, ref, _ string) ([]byte, error) {
		if ref == "HEAD" {
			return []byte("package p\nfunc Fresh() {}\n"), nil
		}
		return nil, errors.New("not present at ref")
	}
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), changedFiles, fileAtRef)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r := NewRegistry()
	RegisterChangedSymbolsTool(r, svc, func(context.Context, string) (string, error) { return "/root", nil }, nil)

	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main",
		// ref_a and ref_b intentionally omitted — defaults trigger.
	})
	if rpcErr != nil {
		t.Fatalf("expected empty-tree fallback to succeed, got %+v", rpcErr)
	}
	var sawFresh bool
	for _, a := range resp.Added {
		if a.Name == "Fresh" {
			sawFresh = true
			break
		}
	}
	if !sawFresh {
		t.Errorf("expected Fresh in added bucket from empty-tree fallback, got %+v", resp.Added)
	}
}

// TestChangedSymbols_ExplicitUnknownRefStillErrors pins that the fallback
// fires only on the implicit-default path; an explicit caller-supplied
// ref that doesn't resolve still surfaces the friendly invalid-params
// error (caller typo, stale branch name, etc.).
func TestChangedSymbols_ExplicitUnknownRefStillErrors(t *testing.T) {
	changedFiles := func(_ context.Context, _, refA, _ string) ([]string, error) {
		return nil, fmt.Errorf("%w: refs=%s..HEAD", gitinfra.ErrUnknownRevision, refA)
	}
	fileAtRef := func(_ context.Context, _, _, _ string) ([]byte, error) {
		return nil, errors.New("not present at ref")
	}
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), changedFiles, fileAtRef)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r := NewRegistry()
	RegisterChangedSymbolsTool(r, svc, func(context.Context, string) (string, error) { return "/root", nil }, nil)

	_, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main",
		"ref_a": "no-such-branch", "ref_b": "HEAD",
	})
	if rpcErr == nil {
		t.Fatal("expected InvalidParams for explicit unknown ref")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected InvalidParams, got %d", rpcErr.Code)
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
