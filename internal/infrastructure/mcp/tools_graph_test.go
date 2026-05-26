package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ---------------------------------------------------------------------------
// Stub GraphStorage
// ---------------------------------------------------------------------------

// stubGraphStorage is an in-test implementation of ports.GraphStorage.
type stubGraphStorage struct {
	nodes map[string]*domain.Node // keyed by nodeID
	edges []*domain.Edge
	graph *domain.Graph
}

func newStubGraphStorage() *stubGraphStorage {
	return &stubGraphStorage{
		nodes: make(map[string]*domain.Node),
	}
}

func (s *stubGraphStorage) addNode(n *domain.Node) {
	s.nodes[string(n.ID)] = n
}

func (s *stubGraphStorage) addEdge(e *domain.Edge) {
	s.edges = append(s.edges, e)
}

func (s *stubGraphStorage) SaveNode(_ context.Context, _, _ string, n *domain.Node) error {
	s.nodes[string(n.ID)] = n
	return nil
}

func (s *stubGraphStorage) SaveEdge(_ context.Context, _, _ string, e *domain.Edge) error {
	s.edges = append(s.edges, e)
	return nil
}

func (s *stubGraphStorage) DeleteFile(_ context.Context, _, _, _ string) error { return nil }

func (s *stubGraphStorage) LoadGraph(_ context.Context, repoID, branch string) (*domain.Graph, error) {
	if s.graph != nil {
		return s.graph, nil
	}
	g, err := domain.NewGraph(repoID, branch)
	if err != nil {
		return nil, err
	}
	for _, n := range s.nodes {
		_ = g.AddNode(n)
	}
	for _, e := range s.edges {
		_ = g.AddEdge(e)
	}
	return g, nil
}

func (s *stubGraphStorage) FindNodes(_ context.Context, _, _, symbolName string) ([]*domain.Node, error) {
	var result []*domain.Node
	for _, n := range s.nodes {
		if n.Name == symbolName {
			result = append(result, n)
		}
	}
	return result, nil
}

func (s *stubGraphStorage) GetNode(_ context.Context, _, _ string, id domain.NodeID) (*domain.Node, error) {
	n, ok := s.nodes[string(id)]
	if !ok {
		return nil, nil
	}
	return n, nil
}

func (s *stubGraphStorage) FindNodeByID(_ context.Context, id domain.NodeID) (*domain.Node, error) {
	n, ok := s.nodes[string(id)]
	if !ok {
		return nil, nil
	}
	return n, nil
}

// NodesForFile implements the optional fileQuerier extension used by
// makeGetFileNodesHandler when the storage implements it.
func (s *stubGraphStorage) NodesForFile(_ context.Context, _, _, filePath string) ([]*domain.Node, error) {
	var result []*domain.Node
	for _, n := range s.nodes {
		if n.Path == filePath {
			result = append(result, n)
		}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustNode(t *testing.T, id, path, name string, kind domain.NodeKind) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(id, path, name, kind)
	if err != nil {
		t.Fatalf("NewNode(%q): %v", id, err)
	}
	return n
}

func mustEdge(t *testing.T, src, tgt domain.NodeID, kind domain.EdgeKind) *domain.Edge {
	t.Helper()
	e, err := domain.NewEdge(src, tgt, kind)
	if err != nil {
		t.Fatalf("NewEdge(%q->%q): %v", src, tgt, err)
	}
	return e
}

func dispatchGraph(t *testing.T, r *Registry, method string, params any) (GraphResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{Method: method, Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return GraphResponse{}, rpcErr
	}
	// Re-encode and decode into GraphResponse to handle the any return type.
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var resp GraphResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal GraphResponse: %v", err)
	}
	return resp, nil
}

// dispatchCallChain dispatches eng_get_call_chain and decodes the
// callChainResponse envelope (nodes + edges + cross-repo edges).
func dispatchCallChain(t *testing.T, r *Registry, method string, params any) (callChainResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := &Request{Method: method, Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return callChainResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var resp callChainResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal callChainResponse: %v", err)
	}
	return resp, nil
}

// TestFindSymbol_UnknownRepoIDErrors pins solov2-5rh: an unknown repo_id
// must return a loud NotFound error, not a silently-empty result, so a
// stale/typo'd id is distinguishable from a genuine no-match.
func TestFindSymbol_UnknownRepoIDErrors(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: sampleRepos}))

	_, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "does-not-exist",
		"branch":  "main",
	})
	if rpcErr == nil {
		t.Fatal("expected NotFound error for unknown repo_id, got nil")
		return
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected CodeNotFound (%d), got %d: %s", CodeNotFound, rpcErr.Code, rpcErr.Message)
	}
}

// TestFindSymbol_AmbiguousPrefixRejected guards solov2-rkbc: a 4+ char prefix
// that matches multiple repos must return a clear ambiguous-prefix error
// instead of silently picking one.
func TestFindSymbol_AmbiguousPrefixRejected(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))
	repos := []application.RepoRecord{
		{RepoID: "deadbeef111111111111111111111111", RootPath: "/p1", ActiveBranch: "main"},
		{RepoID: "deadbeef222222222222222222222222", RootPath: "/p2", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))
	_, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "deadbeef", // 8 chars but matches both
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams for ambiguous prefix, got %+v", rpcErr)
	}
}

// TestFindSymbol_ArbitraryPrefixAccepted guards solov2-rkbc: a 4+ char prefix
// that unambiguously matches one repo resolves like the full id (README
// contract).
func TestFindSymbol_ArbitraryPrefixAccepted(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))
	repos := []application.RepoRecord{
		{RepoID: "deadbeefcafebabe1111222233334444", RootPath: "/p", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))
	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "deadbeef", // 8-char prefix, not exact short_id length
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("expected 8-char prefix to resolve, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(resp.Nodes))
	}
}

// TestFindSymbol_ShortRepoIDAccepted pins solov2-d2x: a 12-char short_id
// prefix resolves to the full repo_id.
func TestFindSymbol_ShortRepoIDAccepted(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "0123456789abcdef0123456789abcdef", RootPath: "/p", ActiveBranch: "main", LastPromotedSHA: "x"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "0123456789ab", // 12-char short_id
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error for short repo_id: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node via short_id, got %d", len(resp.Nodes))
	}
}

// TestFindSymbol_BranchDefaultsToActiveBranch guards solov2-5vu1: when the
// caller omits branch, the handler resolves it from the registered
// active_branch instead of erroring.
func TestFindSymbol_BranchDefaultsToActiveBranch(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "abcdef0123456789abcdef0123456789", RootPath: "/p", ActiveBranch: "develop"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "abcdef012345",
		// branch intentionally omitted
	})
	if rpcErr != nil {
		t.Fatalf("expected branch auto-resolution, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node with default branch, got %d", len(resp.Nodes))
	}
}

// ---------------------------------------------------------------------------
// eng_find_symbol — finds nodes from graph store
// ---------------------------------------------------------------------------

func TestFindSymbol_ReturnsNodesFromGraphStore(t *testing.T) {
	store := newStubGraphStorage()
	n := mustNode(t, "node-1", "pkg/foo.go", "Foo", domain.KindFunction)
	store.addNode(n)

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].NodeID != "node-1" {
		t.Errorf("expected node-1, got %q", resp.Nodes[0].NodeID)
	}
}

// TestFindSymbol_RanksDeclarationAboveContainer pins solov2-rd0l: when a name
// matches both a package and a function (Go 'package main' + 'func main'), the
// callable declaration must rank first so nodes[0] is usable for
// call_chain/blast_radius.
func TestFindSymbol_RanksDeclarationAboveContainer(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "pkg-main", "main.go", "main", domain.KindPackage))
	store.addNode(mustNode(t, "func-main", "main.go", "main", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol": "main", "repo_id": "repo1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].NodeID != "func-main" {
		t.Errorf("nodes[0] = %q (kind %q), want the function node func-main", resp.Nodes[0].NodeID, resp.Nodes[0].Kind)
	}
}

// ---------------------------------------------------------------------------
// eng_find_symbol — staging overlay overrides promoted node
// ---------------------------------------------------------------------------

func TestFindSymbol_StagingOverridesPromotedNode(t *testing.T) {
	store := newStubGraphStorage()
	promoted := mustNode(t, "node-1", "pkg/foo.go", "Foo", domain.KindFunction)
	store.addNode(promoted)

	staged := mustNode(t, "node-1", "pkg/foo.go", "Foo", domain.KindMethod) // same ID, different kind
	staging := application.NewStagingArea()
	staging.StageFile("repo1", "main", "pkg/foo.go", []*domain.Node{staged}, nil)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging)

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if !resp.IncludedStaging {
		t.Error("expected IncludedStaging=true")
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node after merge, got %d", len(resp.Nodes))
	}
	// Staged version should win.
	if resp.Nodes[0].Kind != string(domain.KindMethod) {
		t.Errorf("expected staged kind %q, got %q", domain.KindMethod, resp.Nodes[0].Kind)
	}
}

// ---------------------------------------------------------------------------
// eng_get_node — found → single node response
// ---------------------------------------------------------------------------

func TestGetNode_Found(t *testing.T) {
	store := newStubGraphStorage()
	n := mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct)
	store.addNode(n)

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "node-42",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].NodeID != "node-42" {
		t.Errorf("wrong node: %q", resp.Nodes[0].NodeID)
	}
}

// ---------------------------------------------------------------------------
// eng_get_node — not found → -32602
// ---------------------------------------------------------------------------

func TestGetNode_NotFound(t *testing.T) {
	store := newStubGraphStorage()
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	_, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "does-not-exist",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr == nil {
		t.Fatal("expected RPCError for not-found node")
		return
	}
	// solov2-byxy: not-found is a domain error (CodeNotFound), not -32602.
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected code %d, got %d", CodeNotFound, rpcErr.Code)
	}
}

// TestGetNode_RepoIDPresentButUnknownRejected pins solov2-hb2s: when the
// caller supplies repo_id (even without branch), the handler must validate
// it against the registry. Previously, an unknown or mistyped repo_id was
// silently ignored — the handler took the cross-repo fallback path (since
// branch was empty) and returned a node from any repo, with no error.
// That made the repo_id parameter advisory in a way the README contract
// did not document.
func TestGetNode_RepoIDPresentButUnknownRejected(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	_, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "node-42",
		"repo_id": "deadbeefdead", // valid 12-char shape, but no such repo
	})
	if rpcErr == nil {
		t.Fatal("expected NotFound for unknown repo_id, got success")
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected CodeNotFound for unknown repo_id, got %d (%s)", rpcErr.Code, rpcErr.Message)
	}
}

// TestGetNode_RepoIDWithoutBranchResolvesActiveBranch pins solov2-hb2s:
// when only repo_id is supplied, the handler must fill branch from the
// registered active_branch and take the scoped GetNode path (not the
// cross-repo fallback). We assert this by registering a repo that
// resolves cleanly — the call must succeed without erroring out of the
// resolveRepoID validation that the fallback path would have skipped.
func TestGetNode_RepoIDWithoutBranchResolvesActiveBranch(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "develop"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "node-42",
		"repo_id": "aaaaaaaaaaaa", // valid short_id
	})
	if rpcErr != nil {
		t.Fatalf("expected scoped lookup to succeed, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "node-42" {
		t.Fatalf("expected node-42, got %+v", resp.Nodes)
	}
}

// TestGetNode_OmitRepoIDAndBranch guards solov2-v4ob: node_id is a globally
// unique content hash, so repo_id and branch must be optional. When both are
// omitted the handler falls back to FindNodeByID (cross-repo lookup).
func TestGetNode_OmitRepoIDAndBranch(t *testing.T) {
	store := newStubGraphStorage()
	n := mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct)
	store.addNode(n)

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "node-42",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "node-42" {
		t.Fatalf("expected node-42, got %+v", resp.Nodes)
	}
}

// ---------------------------------------------------------------------------
// eng_get_call_chain — traverses CALLS edges up to depth
// ---------------------------------------------------------------------------

func TestGetCallChain_TraversesCallsEdges(t *testing.T) {
	store := newStubGraphStorage()
	// Build: A -> B -> C
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	c := mustNode(t, "c", "pkg/c.go", "C", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addNode(c)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))
	store.addEdge(mustEdge(t, "b", "c", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   3,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

	// Should include b and c (reachable via CALLS from a), possibly a itself.
	nodeIDs := make(map[string]bool)
	for _, n := range resp.Nodes {
		nodeIDs[n.NodeID] = true
	}
	if !nodeIDs["b"] {
		t.Error("expected node b in call chain")
	}
	if !nodeIDs["c"] {
		t.Error("expected node c in call chain")
	}
	if len(resp.Edges) == 0 {
		t.Error("expected at least one edge in response")
	}
}

// TestGetCallChain_AcceptsSymbol guards solov2-lcz6: callers can pass
// 'symbol' instead of 'node_id' for parity with eng_find_symbol.
func TestGetCallChain_AcceptsSymbol(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "Alpha", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "Beta", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"symbol":  "Alpha",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   2,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	found := false
	for _, n := range resp.Nodes {
		if n.NodeID == "b" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected node b reached via symbol=Alpha, got %+v", resp.Nodes)
	}
}

// ---------------------------------------------------------------------------
// eng_get_call_chain — depth > 10 → -32602
// ---------------------------------------------------------------------------

func TestGetCallChain_DepthTooLarge(t *testing.T) {
	store := newStubGraphStorage()
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	_, rpcErr := dispatchGraph(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   11,
	})
	if rpcErr == nil {
		t.Fatal("expected RPCError for depth > 10")
		return
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
}

// ---------------------------------------------------------------------------
// eng_get_file_nodes — returns staged nodes when present
// ---------------------------------------------------------------------------

func TestGetFileNodes_ReturnsStagedNodesWhenPresent(t *testing.T) {
	store := newStubGraphStorage()
	// Put a promoted node in the store.
	promoted := mustNode(t, "p1", "pkg/foo.go", "OldFunc", domain.KindFunction)
	store.addNode(promoted)

	// Stage a different node for the same file.
	staged := mustNode(t, "s1", "pkg/foo.go", "NewFunc", domain.KindFunction)
	staging := application.NewStagingArea()
	staging.StageFile("repo1", "main", "pkg/foo.go", []*domain.Node{staged}, nil)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging)

	resp, rpcErr := dispatchGraph(t, r, "eng_get_file_nodes", map[string]string{
		"file_path": "pkg/foo.go",
		"repo_id":   "repo1",
		"branch":    "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if !resp.IncludedStaging {
		t.Error("expected IncludedStaging=true when staged nodes are present")
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 staged node, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].NodeID != "s1" {
		t.Errorf("expected staged node s1, got %q", resp.Nodes[0].NodeID)
	}
}

// ---------------------------------------------------------------------------
// eng_get_file_nodes — falls back to promoted store when not staged
// ---------------------------------------------------------------------------

func TestGetFileNodes_FallsBackToPromotedStore(t *testing.T) {
	store := newStubGraphStorage()
	n1 := mustNode(t, "n1", "pkg/promoted.go", "PromotedFunc", domain.KindFunction)
	n2 := mustNode(t, "n2", "pkg/other.go", "OtherFunc", domain.KindFunction)
	store.addNode(n1)
	store.addNode(n2)

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea()) // no staging

	resp, rpcErr := dispatchGraph(t, r, "eng_get_file_nodes", map[string]string{
		"file_path": "pkg/promoted.go",
		"repo_id":   "repo1",
		"branch":    "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if resp.IncludedStaging {
		t.Error("expected IncludedStaging=false when no staging is present")
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 promoted node, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].NodeID != "n1" {
		t.Errorf("expected node n1, got %q", resp.Nodes[0].NodeID)
	}
}

// TestGetFileNodes_ResolvesRelativePath verifies a repo-relative file_path is
// joined to the repo root before lookup, instead of silently matching nothing
// (solov2-829). Also exercises the "path" alias (solov2-3p1).
func TestGetFileNodes_ResolvesRelativePath(t *testing.T) {
	store := newStubGraphStorage()
	n1 := mustNode(t, "n1", "/abs/repo/internal/server.go", "Serve", domain.KindFunction)
	store.addNode(n1)

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: []application.RepoRecord{{RepoID: "repo1", RootPath: "/abs/repo"}}}))

	resp, rpcErr := dispatchGraph(t, r, "eng_get_file_nodes", map[string]string{
		"path":    "internal/server.go", // relative + alias
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "n1" {
		t.Fatalf("expected node n1 from resolved relative path, got %+v", resp.Nodes)
	}
}

// TestFindSymbol_ResolvesRepoFromCwdWhenOmitted guards solov2-ktz0: when
// repo_id is omitted but the shim-injected cwd matches a registered repo's
// RootPath (or sits inside one), the handler resolves to that repo instead
// of rejecting with "repo_id is required". Critical for multi-repo users.
func TestFindSymbol_ResolvesRepoFromCwdWhenOmitted(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n-cwd", "/home/u/projects/alpha/main.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/home/u/projects/alpha", ActiveBranch: "main"},
		{RepoID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootPath: "/home/u/projects/beta", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	// repo_id omitted, but cwd is inside alpha — should resolve.
	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol": "Foo",
		"cwd":    "/home/u/projects/alpha/sub/dir",
	})
	if rpcErr != nil {
		t.Fatalf("expected cwd-based resolution, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "n-cwd" {
		t.Fatalf("expected n-cwd, got %+v", resp.Nodes)
	}
}

// TestFindSymbol_FansOutWhenRepoIDOmittedAndCwdMismatch pins solov2-g8fh:
// when repo_id is omitted and cwd doesn't match any registered repo, the
// handler fans out across every registered repo instead of erroring. The
// README's "60 second sanity check" example works without naming a repo
// id when the user just spawned veska-mcp from /tmp or similar.
func TestFindSymbol_FansOutWhenRepoIDOmittedAndCwdMismatch(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n-alpha", "/home/u/projects/alpha/main.go", "Foo", domain.KindFunction))
	store.addNode(mustNode(t, "n-beta", "/home/u/projects/beta/lib.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/home/u/projects/alpha", ActiveBranch: "main"},
		{RepoID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootPath: "/home/u/projects/beta", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol": "Foo",
		"cwd":    "/tmp/somewhere/else",
	})
	if rpcErr != nil {
		t.Fatalf("expected fanout success, got %+v", rpcErr)
	}
	// Both repos searched (the stub's FindNodes ignores repo_id so each
	// fanout target returns both nodes; the (repo_id,node_id) merge key
	// dedupes to 2 entries per repo = 4 hits total).
	if len(resp.Nodes) == 0 {
		t.Fatalf("expected nodes from fanout, got empty result")
	}
	// repo_id MUST be populated on every hit when fanout is engaged, so
	// callers can disambiguate which repo each hit belongs to.
	for i, n := range resp.Nodes {
		if n.RepoID == "" {
			t.Errorf("nodes[%d] missing repo_id on fanout response: %+v", i, n)
		}
	}
}

// TestFindSymbol_NoFanoutWhenSingleRepoSoNoRepoIDLeaks pins solov2-g8fh: a
// single-repo install must keep the pre-fanout wire shape — `repo_id` is
// only emitted when the response actually crosses repos.
func TestFindSymbol_NoFanoutWhenSingleRepoSoNoRepoIDLeaks(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "/abs/repo/main.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{"symbol": "Foo"})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(resp.Nodes))
	}
	if resp.Nodes[0].RepoID != "" {
		t.Errorf("single-repo response leaked repo_id=%q (must be omitted)", resp.Nodes[0].RepoID)
	}
}

// TestFindSymbol_NoReposRegisteredStillErrors guards the empty-registry
// edge case — fanout has nothing to span, so the original "no repos
// registered" message must still surface.
func TestFindSymbol_NoReposRegisteredStillErrors(t *testing.T) {
	store := newStubGraphStorage()
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: nil}))

	_, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{"symbol": "Foo"})
	if rpcErr == nil {
		t.Fatal("expected error when no repos registered")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

// TestGetFileNodes_BranchDefaultsToActiveBranch guards solov2-gp2k: when the
// caller omits branch, the handler resolves it from the registered
// active_branch instead of erroring — matching find_symbol et al.
func TestGetFileNodes_BranchDefaultsToActiveBranch(t *testing.T) {
	store := newStubGraphStorage()
	n1 := mustNode(t, "n1", "/abs/repo/pkg/promoted.go", "PromotedFunc", domain.KindFunction)
	store.addNode(n1)

	repos := []application.RepoRecord{
		{RepoID: "abcdef0123456789abcdef0123456789", RootPath: "/abs/repo", ActiveBranch: "develop"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_get_file_nodes", map[string]string{
		"file_path": "/abs/repo/pkg/promoted.go",
		"repo_id":   "abcdef012345",
		// branch intentionally omitted
	})
	if rpcErr != nil {
		t.Fatalf("expected branch auto-resolution, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "n1" {
		t.Fatalf("expected node n1 with default branch, got %+v", resp.Nodes)
	}
}

// ---------------------------------------------------------------------------
// p95 benchmark — eng_find_symbol against 1000-node in-memory stub
// ---------------------------------------------------------------------------

func BenchmarkFindSymbol(b *testing.B) {
	store := newStubGraphStorage()
	// Seed 1000 nodes with names like "Symbol0" … "Symbol999".
	for i := range 1000 {
		id := fmt.Sprintf("node-%d", i)
		name := fmt.Sprintf("Symbol%d", i)
		n, err := domain.NewNode(id, "pkg/gen.go", name, domain.KindFunction)
		if err != nil {
			b.Fatalf("NewNode: %v", err)
		}
		store.addNode(n)
	}

	r := NewRegistry()
	RegisterGraphTools(r, store, application.NewStagingArea())

	rng := rand.New(rand.NewSource(42))
	params := func() json.RawMessage {
		i := rng.Intn(1000)
		raw, _ := json.Marshal(map[string]string{
			"symbol":  fmt.Sprintf("Symbol%d", i),
			"repo_id": "repo1",
			"branch":  "main",
		})
		return raw
	}

	latencies := make([]time.Duration, b.N)

	b.ResetTimer()
	for i := range b.N {
		p := params()
		req := &Request{Method: "eng_find_symbol", Params: p}
		start := time.Now()
		_, _ = r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
		latencies[i] = time.Since(start)
	}
	b.StopTimer()

	// Compute and report p95.
	slices.Sort(latencies)
	p95idx := int(float64(len(latencies)) * 0.95)
	if p95idx >= len(latencies) {
		p95idx = len(latencies) - 1
	}
	p95 := latencies[p95idx]
	b.ReportMetric(float64(p95.Microseconds()), "p95_us")

	if p95 > 50*time.Millisecond {
		b.Errorf("p95 latency %v exceeds 50ms budget", p95)
	}
}
