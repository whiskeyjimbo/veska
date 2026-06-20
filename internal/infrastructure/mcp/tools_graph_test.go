// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
)

type stubGraphStorage struct {
	nodes map[string]*domain.Node
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

func (s *stubGraphStorage) FindNodeIDsByPrefix(_ context.Context, prefix string, limit int) ([]domain.NodeID, error) {
	var out []domain.NodeID
	for id := range s.nodes {
		if strings.HasPrefix(id, prefix) {
			out = append(out, domain.NodeID(id))
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (s *stubGraphStorage) NodesForFile(_ context.Context, _, _, filePath string) ([]*domain.Node, error) {
	var result []*domain.Node
	for _, n := range s.nodes {
		if n.Path == filePath {
			result = append(result, n)
		}
	}
	return result, nil
}

// GetNodeSnippet returns the snippet raw content associated with the stub node ID.
func (s *stubGraphStorage) GetNodeSnippet(_ context.Context, _, _ string, id domain.NodeID) (string, error) {
	n, ok := s.nodes[string(id)]
	if !ok || n.RawContent == nil {
		return "", nil
	}
	return *n.RawContent, nil
}

func mustNode(t *testing.T, id, path, name string, kind domain.NodeKind) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind})
	if err != nil {
		t.Fatalf("NewNode(NodeSpec{ID: %q}): %v", id, err)
	}
	return n
}

func mustEdge(t *testing.T, src, tgt domain.NodeID, kind domain.EdgeKind) *domain.Edge {
	t.Helper()
	e, err := domain.NewEdge(domain.EdgeSpec{Src: src, Tgt: tgt, Kind: kind})
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

// An unknown repo ID must return CodeNotFound rather than a silently empty result.
func TestFindSymbol_UnknownRepoIDErrors(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
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

// If a repository ID prefix matches multiple repositories, the search must fail with CodeInvalidParams.
func TestFindSymbol_AmbiguousPrefixRejected(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))
	repos := []application.RepoRecord{
		{RepoID: "deadbeef111111111111111111111111", RootPath: "/p1", ActiveBranch: "main"},
		{RepoID: "deadbeef222222222222222222222222", RootPath: "/p2", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))
	_, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "deadbeef",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams for ambiguous prefix, got %+v", rpcErr)
	}
}

// An unambiguous repository ID prefix of at least 4 characters successfully resolves to the target repository.
func TestFindSymbol_ArbitraryPrefixAccepted(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))
	repos := []application.RepoRecord{
		{RepoID: "deadbeefcafebabe1111222233334444", RootPath: "/p", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))
	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "deadbeef",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("expected 8-char prefix to resolve, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(resp.Nodes))
	}
}

// A 12-character repository ID prefix resolves to the target repository.
func TestFindSymbol_ShortRepoIDAccepted(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "0123456789abcdef0123456789abcdef", RootPath: "/p", ActiveBranch: "main", LastPromotedSHA: "x"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "0123456789ab",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error for short repo_id: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node via short_id, got %d", len(resp.Nodes))
	}
}

// If the branch parameter is omitted, the query defaults to the repository's active branch.
func TestFindSymbol_BranchDefaultsToActiveBranch(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "abcdef0123456789abcdef0123456789", RootPath: "/p", ActiveBranch: "develop"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "abcdef012345",
	})
	if rpcErr != nil {
		t.Fatalf("expected branch auto-resolution, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node with default branch, got %d", len(resp.Nodes))
	}
}

func TestFindSymbol_ReturnsNodesFromGraphStore(t *testing.T) {
	store := newStubGraphStorage()
	n := mustNode(t, "node-1", "pkg/foo.go", "Foo", domain.KindFunction)
	store.addNode(n)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

// When a query matches both a container and a declaration, the declaration is ranked higher in the results.
func TestFindSymbol_RanksDeclarationAboveContainer(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "pkg-main", "main.go", "main", domain.KindPackage))
	store.addNode(mustNode(t, "func-main", "main.go", "main", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

func TestFindSymbol_StagingOverridesPromotedNode(t *testing.T) {
	store := newStubGraphStorage()
	promoted := mustNode(t, "node-1", "pkg/foo.go", "Foo", domain.KindFunction)
	store.addNode(promoted)

	staged := mustNode(t, "node-1", "pkg/foo.go", "Foo", domain.KindMethod)
	area := staging.NewArea()
	area.Stage("repo1", "main", "pkg/foo.go", staging.File{Nodes: []*domain.Node{staged}, Edges: nil})

	r := NewRegistry()
	RegisterGraphTools(r, store, area)

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
	if resp.Nodes[0].Kind != string(domain.KindMethod) {
		t.Errorf("expected staged kind %q, got %q", domain.KindMethod, resp.Nodes[0].Kind)
	}
}

func TestGetNode_Found(t *testing.T) {
	store := newStubGraphStorage()
	n := mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct)
	store.addNode(n)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

func TestGetNode_NotFound(t *testing.T) {
	store := newStubGraphStorage()
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	_, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "does-not-exist",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr == nil {
		t.Fatal("expected RPCError for not-found node")
		return
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected code %d, got %d", CodeNotFound, rpcErr.Code)
	}
}

// If a repository ID is provided but cannot be found, the request fails with CodeNotFound even if the node exists globally.
func TestGetNode_RepoIDPresentButUnknownRejected(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	_, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "node-42",
		"repo_id": "deadbeefdead",
	})
	if rpcErr == nil {
		t.Fatal("expected NotFound for unknown repo_id, got success")
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected CodeNotFound for unknown repo_id, got %d (%s)", rpcErr.Code, rpcErr.Message)
	}
}

// When only repo_id is provided, the handler resolves the repository's active branch to scope the search.
func TestGetNode_RepoIDWithoutBranchResolvesActiveBranch(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "develop"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "node-42",
		"repo_id": "aaaaaaaaaaaa",
	})
	if rpcErr != nil {
		t.Fatalf("expected scoped lookup to succeed, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "node-42" {
		t.Fatalf("expected node-42, got %+v", resp.Nodes)
	}
}

// If both repository ID and branch are omitted, the query falls back to a global lookup across all repositories.
func TestGetNode_OmitRepoIDAndBranch(t *testing.T) {
	store := newStubGraphStorage()
	n := mustNode(t, "node-42", "pkg/bar.go", "Bar", domain.KindStruct)
	store.addNode(n)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

// Node IDs can be queried using a unique 12-character prefix of the node's content hash.
func TestGetNode_ResolvesUniquePrefix(t *testing.T) {
	store := newStubGraphStorage()
	full := "f470f8ff4243aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.addNode(mustNode(t, full, "pkg/bar.go", "Add", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "f470f8ff4243",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != full {
		t.Fatalf("prefix did not resolve to full node: %+v", resp.Nodes)
	}
}

// A full 64-character node ID successfully resolves without being treated as an ambiguous prefix.
func TestGetNode_FullIDStillResolves(t *testing.T) {
	store := newStubGraphStorage()
	full := "f470f8ff4243aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.addNode(mustNode(t, full, "pkg/bar.go", "Add", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": full,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != full {
		t.Fatalf("full id did not resolve: %+v", resp.Nodes)
	}
}

// If a node ID prefix matches multiple nodes, the query fails with CodeInvalidParams listing all matching candidates.
func TestGetNode_AmbiguousPrefixErrorsWithCandidates(t *testing.T) {
	store := newStubGraphStorage()
	id1 := "dead000011110000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id2 := "dead000022220000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.addNode(mustNode(t, id1, "a.go", "Fn", domain.KindFunction))
	store.addNode(mustNode(t, id2, "b.go", "Fn", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	_, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "dead0000",
	})
	if rpcErr == nil {
		t.Fatal("expected ambiguity error, got success")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, id1) || !strings.Contains(rpcErr.Message, id2) {
		t.Errorf("ambiguity message must list candidate ids; got %q", rpcErr.Message)
	}
}

// Inputs shorter than the prefix minimum length are matched exactly to avoid expensive scans over short keys.
func TestGetNode_ShortInputTreatedAsExact(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "deadbeefcafef00d", "a.go", "Fn", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	_, rpcErr := dispatchGraph(t, r, "eng_get_node", map[string]string{
		"node_id": "dead",
	})
	if rpcErr == nil {
		t.Fatal("expected not-found for too-short non-exact input, got success")
	}
	if rpcErr.Code != CodeNotFound {
		t.Errorf("expected CodeNotFound, got %d (%s)", rpcErr.Code, rpcErr.Message)
	}
}

func TestGetCallChain_TraversesCallsEdges(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	c := mustNode(t, "c", "pkg/c.go", "C", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addNode(c)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))
	store.addEdge(mustEdge(t, "b", "c", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]any{
		"node_id": "a",
		"repo_id": "repo1",
		"branch":  "main",
		"depth":   3,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}

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

// Callers can query call chains using a symbol name instead of a direct node ID.
func TestGetCallChain_AcceptsSymbol(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "Alpha", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "Beta", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, "a", "b", domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

func TestGetCallChain_DepthTooLarge(t *testing.T) {
	store := newStubGraphStorage()
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

// If a callable node has no resolved edges, the response includes a degraded reason suggesting chained selectors may be unresolved.
func TestGetCallChain_EmptyEdgesOnCallableEmitsChainedSelectorsHint(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "fn-lonely", "pkg/x.go", "Lonely", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]string{
		"node_id": "fn-lonely",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Edges) != 0 {
		t.Fatalf("expected zero edges for lonely callable; got %d", len(resp.Edges))
	}
	var sawHint bool
	if slices.Contains(resp.DegradedReasons, protocol.DegradedReasonChainedSelectorsUnresolved) {
		sawHint = true
	}
	if !sawHint {
		t.Errorf("expected %q in degraded_reasons; got %+v", protocol.DegradedReasonChainedSelectorsUnresolved, resp.DegradedReasons)
	}
}

// The chained selectors degraded hint is suppressed if the call chain contains at least one edge.
func TestGetCallChain_EdgesPresentSuppressesHint(t *testing.T) {
	store := newStubGraphStorage()
	a := mustNode(t, "a", "pkg/a.go", "A", domain.KindFunction)
	b := mustNode(t, "b", "pkg/b.go", "B", domain.KindFunction)
	store.addNode(a)
	store.addNode(b)
	store.addEdge(mustEdge(t, a.ID, b.ID, domain.EdgeCalls))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]string{
		"node_id": "a", "repo_id": "repo1", "branch": "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	for _, r := range resp.DegradedReasons {
		if r == protocol.DegradedReasonChainedSelectorsUnresolved {
			t.Errorf("hint must not fire when edges resolved: %+v", resp.DegradedReasons)
		}
	}
}

// If a function body only calls unmodeled external dependencies, we report external_callees_only instead of chained_selectors_unresolved.
func TestGetCallChain_StdlibOnlyBodyEmitsExternalCalleesReason(t *testing.T) {
	store := newStubGraphStorage()
	raw := `func (g *Greeter) Greet(name string) string {
	if name == "" { name = "world" }
	return fmt.Sprintf("%s, %s!", g.Prefix, strings.TrimSpace(name))
}`
	seed, err := domain.NewNode(domain.NodeSpec{ID: "fn-greet", Path: "libfoo/greeter.go", Name: "Greeter.Greet", Kind: domain.KindMethod}, domain.WithRawContent(raw))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	store.addNode(seed)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]string{
		"node_id": "fn-greet",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Edges) != 0 {
		t.Fatalf("expected zero edges; got %d", len(resp.Edges))
	}
	if slices.Contains(resp.DegradedReasons, protocol.DegradedReasonChainedSelectorsUnresolved) {
		t.Errorf("chained_selectors_unresolved must NOT fire on a body with no chained selectors; got %+v", resp.DegradedReasons)
	}
	if !slices.Contains(resp.DegradedReasons, protocol.DegradedReasonExternalCalleesOnly) {
		t.Errorf("expected %q in degraded_reasons; got %+v", protocol.DegradedReasonExternalCalleesOnly, resp.DegradedReasons)
	}
}

// We verify that the chained selectors degraded hint continues to fire on bodies containing chained selector invocations.
func TestGetCallChain_ChainedSelectorBodyStillEmitsChainedHint(t *testing.T) {
	store := newStubGraphStorage()
	raw := `func (s *Server) Start() error {
	s.router.handlers.Register("/x", h)
	return s.config.tls.Load()
}`
	seed, err := domain.NewNode(domain.NodeSpec{ID: "fn-start", Path: "srv/server.go", Name: "Server.Start", Kind: domain.KindMethod}, domain.WithRawContent(raw))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	store.addNode(seed)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchCallChain(t, r, "eng_get_call_chain", map[string]string{
		"node_id": "fn-start",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if !slices.Contains(resp.DegradedReasons, protocol.DegradedReasonChainedSelectorsUnresolved) {
		t.Errorf("expected %q on a body with chained selectors; got %+v", protocol.DegradedReasonChainedSelectorsUnresolved, resp.DegradedReasons)
	}
}

type stubScanTracker struct {
	scans []application.ScanState
}

func (s *stubScanTracker) IsAnyScanRunning() bool { return len(s.scans) > 0 }
func (s *stubScanTracker) Snapshot() []application.ScanState {
	out := make([]application.ScanState, len(s.scans))
	copy(out, s.scans)
	return out
}

// If a symbol query is empty while repository indexing is in progress, the response returns the indexing status in the degraded reasons list.
func TestFindSymbol_EmptyDuringIndexingEmitsHint(t *testing.T) {
	store := newStubGraphStorage()
	tracker := &stubScanTracker{scans: []application.ScanState{
		{RepoID: "scanning-repo", Phase: "walking"},
	}}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithScanTracker(tracker))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "NothingHere",
		"repo_id": "scanning-repo",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("expected empty nodes; got %d", len(resp.Nodes))
	}
	if !slices.Contains(resp.DegradedReasons, protocol.DegradedReasonIndexingInProgress) {
		t.Errorf("expected %q in degraded_reasons; got %+v", protocol.DegradedReasonIndexingInProgress, resp.DegradedReasons)
	}
	if !slices.Contains(resp.IndexingRepos, "scanning-repo") {
		t.Errorf("expected scanning-repo in indexing_repos; got %+v", resp.IndexingRepos)
	}
}

// The indexing in progress degraded hint is suppressed when no repository index scans are running.
func TestFindSymbol_EmptyWithNoScansSuppressesHint(t *testing.T) {
	store := newStubGraphStorage()
	tracker := &stubScanTracker{}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithScanTracker(tracker))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "NothingHere",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if slices.Contains(resp.DegradedReasons, protocol.DegradedReasonIndexingInProgress) {
		t.Errorf("indexing_in_progress must NOT fire when tracker is empty; got %+v", resp.DegradedReasons)
	}
	if len(resp.IndexingRepos) != 0 {
		t.Errorf("indexing_repos must be empty when no scans running; got %+v", resp.IndexingRepos)
	}
}

// The indexing in progress degraded hint is suppressed if the query successfully returns nodes, even if other indexing operations are running.
func TestFindSymbol_NonEmptyDuringIndexingSuppressesHint(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/x.go", "Foo", domain.KindFunction))
	tracker := &stubScanTracker{scans: []application.ScanState{
		{RepoID: "scanning-repo", Phase: "walking"},
	}}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithScanTracker(tracker))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node; got %d", len(resp.Nodes))
	}
	if slices.Contains(resp.DegradedReasons, protocol.DegradedReasonIndexingInProgress) {
		t.Errorf("indexing_in_progress must NOT fire on a non-empty result; got %+v", resp.DegradedReasons)
	}
}

type stubReconcileReader struct {
	reconciling map[string]bool
}

func (s *stubReconcileReader) IsRepoReconciling(repoID string) bool {
	return s.reconciling[repoID]
}

// The wake reconciling degraded reason is attached to the response if the target repository is currently undergoing a reconciliation sweep.
func TestFindSymbol_WakeReconcilingAttachesOnNonEmpty(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/x.go", "Foo", domain.KindFunction))
	rec := &stubReconcileReader{reconciling: map[string]bool{"repoA": true}}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithReconcileTracker(rec))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "repoA",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node; got %d", len(resp.Nodes))
	}
	if !slices.Contains(resp.DegradedReasons, protocol.DegradedReasonWakeReconciling) {
		t.Errorf("expected wake_reconciling on a non-empty result while repoA sweeps; got %+v", resp.DegradedReasons)
	}
	if !slices.Contains(resp.WakeReconcilingRepos, "repoA") {
		t.Errorf("expected repoA in wake_reconciling_repos; got %+v", resp.WakeReconcilingRepos)
	}
}

// The wake reconciling degraded reason is only attached to queries targeting the specific repository undergoing reconciliation.
func TestFindSymbol_WakeReconcilingSuppressedForOtherRepo(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/x.go", "Foo", domain.KindFunction))
	rec := &stubReconcileReader{reconciling: map[string]bool{"repoB": true}}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithReconcileTracker(rec))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "repoA",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if slices.Contains(resp.DegradedReasons, protocol.DegradedReasonWakeReconciling) {
		t.Errorf("wake_reconciling must NOT fire for repoA when only repoB sweeps; got %+v", resp.DegradedReasons)
	}
	if len(resp.WakeReconcilingRepos) != 0 {
		t.Errorf("wake_reconciling_repos must be empty; got %+v", resp.WakeReconcilingRepos)
	}
}

// The wake reconciling degraded reason is attached to empty query results if the repository is undergoing reconciliation.
func TestFindSymbol_WakeReconcilingAttachesOnEmpty(t *testing.T) {
	store := newStubGraphStorage()
	rec := &stubReconcileReader{reconciling: map[string]bool{"repoA": true}}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(), WithReconcileTracker(rec))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "NothingHere",
		"repo_id": "repoA",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("expected empty nodes; got %d", len(resp.Nodes))
	}
	if !slices.Contains(resp.DegradedReasons, protocol.DegradedReasonWakeReconciling) {
		t.Errorf("expected wake_reconciling on empty result while repoA sweeps; got %+v", resp.DegradedReasons)
	}
}

// If no reconciliation tracker is configured, the wake reconciling check is skipped safely.
func TestFindSymbol_WakeReconcilingNilReaderNoOp(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "pkg/x.go", "Foo", domain.KindFunction))

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol":  "Foo",
		"repo_id": "repoA",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if slices.Contains(resp.DegradedReasons, protocol.DegradedReasonWakeReconciling) {
		t.Errorf("wake_reconciling must NOT fire with a nil reader; got %+v", resp.DegradedReasons)
	}
}

func TestGetFileNodes_ReturnsStagedNodesWhenPresent(t *testing.T) {
	store := newStubGraphStorage()
	promoted := mustNode(t, "p1", "pkg/foo.go", "OldFunc", domain.KindFunction)
	store.addNode(promoted)

	staged := mustNode(t, "s1", "pkg/foo.go", "NewFunc", domain.KindFunction)
	area := staging.NewArea()
	area.Stage("repo1", "main", "pkg/foo.go", staging.File{Nodes: []*domain.Node{staged}, Edges: nil})

	r := NewRegistry()
	RegisterGraphTools(r, store, area)

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

func TestGetFileNodes_FallsBackToPromotedStore(t *testing.T) {
	store := newStubGraphStorage()
	n1 := mustNode(t, "n1", "pkg/promoted.go", "PromotedFunc", domain.KindFunction)
	n2 := mustNode(t, "n2", "pkg/other.go", "OtherFunc", domain.KindFunction)
	store.addNode(n1)
	store.addNode(n2)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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

// Absolute file paths provided by the caller are automatically relativized to match the repository-relative keys used in storage.
func TestGetFileNodes_ResolvesRelativePath(t *testing.T) {
	store := newStubGraphStorage()
	n1 := mustNode(t, "n1", "internal/server.go", "Serve", domain.KindFunction)
	store.addNode(n1)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: []application.RepoRecord{{RepoID: "repo1", RootPath: "/abs/repo"}}}))

	resp, rpcErr := dispatchGraph(t, r, "eng_get_file_nodes", map[string]string{
		"path":    "/abs/repo/internal/server.go",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "n1" {
		t.Fatalf("expected node n1 from resolved absolute path, got %+v", resp.Nodes)
	}
}

// If the repository ID is omitted, we attempt to resolve the repository automatically using the shim-injected current working directory.
func TestFindSymbol_ResolvesRepoFromCwdWhenOmitted(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n-cwd", "/home/u/projects/alpha/main.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/home/u/projects/alpha", ActiveBranch: "main"},
		{RepoID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootPath: "/home/u/projects/beta", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

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

// If both repository ID and current working directory are missing or unmatched, the query fans out across all registered repositories.
func TestFindSymbol_FansOutWhenRepoIDOmittedAndCwdMismatch(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n-alpha", "/home/u/projects/alpha/main.go", "Foo", domain.KindFunction))
	store.addNode(mustNode(t, "n-beta", "/home/u/projects/beta/lib.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/home/u/projects/alpha", ActiveBranch: "main"},
		{RepoID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootPath: "/home/u/projects/beta", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{
		"symbol": "Foo",
		"cwd":    "/tmp/somewhere/else",
	})
	if rpcErr != nil {
		t.Fatalf("expected fanout success, got %+v", rpcErr)
	}
	if len(resp.Nodes) == 0 {
		t.Fatalf("expected nodes from fanout, got empty result")
	}
	for i, n := range resp.Nodes {
		if n.RepoID == "" {
			t.Errorf("nodes[%d] missing repo_id on fanout response: %+v", i, n)
		}
	}
}

// For environments with exactly one registered repository, repository IDs are omitted from query results to match the single-repository response format.
func TestFindSymbol_NoFanoutWhenSingleRepoSoNoRepoIDLeaks(t *testing.T) {
	store := newStubGraphStorage()
	store.addNode(mustNode(t, "n1", "/abs/repo/main.go", "Foo", domain.KindFunction))

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
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

// If no repositories are registered in the system, querying symbols fails with CodeInvalidParams.
func TestFindSymbol_NoReposRegisteredStillErrors(t *testing.T) {
	store := newStubGraphStorage()
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: nil}))

	_, rpcErr := dispatchGraph(t, r, "eng_find_symbol", map[string]string{"symbol": "Foo"})
	if rpcErr == nil {
		t.Fatal("expected error when no repos registered")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams, got %d", rpcErr.Code)
	}
}

// If the branch parameter is omitted in a file nodes query, the repository's active branch is used as a default.
func TestGetFileNodes_BranchDefaultsToActiveBranch(t *testing.T) {
	store := newStubGraphStorage()
	n1 := mustNode(t, "n1", "pkg/promoted.go", "PromotedFunc", domain.KindFunction)
	store.addNode(n1)

	repos := []application.RepoRecord{
		{RepoID: "abcdef0123456789abcdef0123456789", RootPath: "/abs/repo", ActiveBranch: "develop"},
	}
	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea(),
		WithRepoLister(&stubRepoLister{repos: repos}))

	resp, rpcErr := dispatchGraph(t, r, "eng_get_file_nodes", map[string]string{
		"file_path": "/abs/repo/pkg/promoted.go",
		"repo_id":   "abcdef012345",
	})
	if rpcErr != nil {
		t.Fatalf("expected branch auto-resolution, got %+v", rpcErr)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "n1" {
		t.Fatalf("expected node n1 with default branch, got %+v", resp.Nodes)
	}
}

func BenchmarkFindSymbol(b *testing.B) {
	store := newStubGraphStorage()
	for i := range 1000 {
		id := fmt.Sprintf("node-%d", i)
		name := fmt.Sprintf("Symbol%d", i)
		n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: "pkg/gen.go", Name: name, Kind: domain.KindFunction})
		if err != nil {
			b.Fatalf("NewNode: %v", err)
		}
		store.addNode(n)
	}

	r := NewRegistry()
	RegisterGraphTools(r, store, staging.NewArea())

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
