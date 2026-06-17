// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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
	// The returned file path must be absolute to align with the response format of all other node-emitting tools.
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

// We filter out raw parser chunk nodes and expose only semantic symbol changes, setting a degraded reason if a diff contains only non-symbol updates.
func TestChangedSymbols_FiltersChunkEntries(t *testing.T) {
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
	if len(resp.Added)+len(resp.Removed)+len(resp.Modified) == 0 {
		found := slices.Contains(resp.DegradedReasons, changedsymbols.DegradedReasonNonSymbolChangesOnly)
		if !found {
			t.Errorf("comment-only diff must set degraded_reasons=%q, got %+v",
				changedsymbols.DegradedReasonNonSymbolChangesOnly, resp.DegradedReasons)
		}
	}
}

// We support standard git 'base' and 'head' parameters as aliases for 'ref_a' and 'ref_b' to support natural caller names.
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

// Providing conflicting values for the ref_a and base parameters is rejected with CodeInvalidParams.
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

// Empty change buckets must serialize as empty JSON arrays rather than null.
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

// If revision references are omitted, we default to comparing HEAD~1 with HEAD.
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

// On a single-commit repository where HEAD~1 does not exist, the tool falls back to comparing HEAD against git's canonical empty-tree SHA.
func TestChangedSymbols_SingleCommitRepoFallsBackToEmptyTree(t *testing.T) {
	const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	changedFiles := func(_ context.Context, _, refA, _ string) ([]string, error) {
		if refA == "HEAD~1" {
			return nil, fmt.Errorf("%w: refs=HEAD~1..HEAD", gitinfra.ErrUnknownRevision)
		}
		if refA == emptyTreeSHA {
			return []string{"code.go"}, nil
		}
		return nil, fmt.Errorf("unexpected refA %q", refA)
	}
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

// The empty-tree fallback only activates for implicit defaults; explicit unknown references provided by the caller must fail with CodeInvalidParams.
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

// Every reported symbol change must include its line start and line end ranges.
func TestChangedSymbols_PopulatesLineRanges(t *testing.T) {
	m := csMemFiles{
		"refA:code.go": "package p\n",
		"refB:code.go": "package p\n\nfunc Whisper() string {\n\treturn \"shh\"\n}\n",
	}
	r := newChangedSymbolsRegistry(t, m)
	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main", "ref_a": "refA", "ref_b": "refB",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr)
	}
	if len(resp.Added) != 1 {
		t.Fatalf("added = %+v, want one entry", resp.Added)
	}
	got := resp.Added[0]
	if got.LineStart == 0 || got.LineEnd == 0 || got.LineStart > got.LineEnd {
		t.Errorf("expected populated line range, got start=%d end=%d", got.LineStart, got.LineEnd)
	}
}

// The non_symbol_changes_only degraded reason is suppressed if there is at least one semantic symbol change in the response.
func TestChangedSymbols_NonSymbolHintSuppressedWhenSymbolsChanged(t *testing.T) {
	m := csMemFiles{
		"refA:code.go":  "package p\n",
		"refB:code.go":  "package p\n\nfunc Whisper() string { return \"shh\" }\n",
		"refA:notes.md": "old text\n",
		"refB:notes.md": "new text\n",
	}
	files := func(_ context.Context, _, _, _ string) ([]string, error) {
		return []string{"code.go", "notes.md"}, nil
	}
	svc, err := changedsymbols.NewService(treesitter.NewGoParser(), files, m.fileAtRef)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r := NewRegistry()
	RegisterChangedSymbolsTool(r, svc, func(context.Context, string) (string, error) {
		return "/root", nil
	}, nil)
	resp, rpcErr := dispatchChangedSymbols(t, r, map[string]string{
		"repo_id": "repo1", "branch": "main", "ref_a": "refA", "ref_b": "refB",
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %v", rpcErr)
	}
	if len(resp.Added) == 0 {
		t.Fatalf("expected an added symbol, got resp=%+v", resp)
	}
	if slices.Contains(resp.DegradedReasons, changedsymbols.DegradedReasonNonSymbolChangesOnly) {
		t.Errorf("degraded_reason %q must be suppressed when the symbol diff is non-empty; got %+v",
			changedsymbols.DegradedReasonNonSymbolChangesOnly, resp.DegradedReasons)
	}
}
