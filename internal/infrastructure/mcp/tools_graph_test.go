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
	if string(resp.Nodes[0].ID) != "node-1" {
		t.Errorf("expected node-1, got %q", resp.Nodes[0].ID)
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
	if resp.Nodes[0].Kind != domain.KindMethod {
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
	if string(resp.Nodes[0].ID) != "node-42" {
		t.Errorf("wrong node: %q", resp.Nodes[0].ID)
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
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
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

	resp, rpcErr := dispatchGraph(t, r, "eng_get_call_chain", map[string]any{
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
		nodeIDs[string(n.ID)] = true
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
	if string(resp.Nodes[0].ID) != "s1" {
		t.Errorf("expected staged node s1, got %q", resp.Nodes[0].ID)
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
	if string(resp.Nodes[0].ID) != "n1" {
		t.Errorf("expected node n1, got %q", resp.Nodes[0].ID)
	}
}

// ---------------------------------------------------------------------------
// eng_get_node_as_of — skips staging overlay
// ---------------------------------------------------------------------------

func TestGetNodeAsOf_SkipsStagingOverlay(t *testing.T) {
	store := newStubGraphStorage()
	promoted := mustNode(t, "node-99", "pkg/baz.go", "Baz", domain.KindFunction)
	store.addNode(promoted)

	// Stage a different version of the same node.
	staged := mustNode(t, "node-99", "pkg/baz.go", "Baz", domain.KindMethod)
	staging := application.NewStagingArea()
	staging.StageFile("repo1", "main", "pkg/baz.go", []*domain.Node{staged}, nil)

	r := NewRegistry()
	RegisterGraphTools(r, store, staging)

	resp, rpcErr := dispatchGraph(t, r, "eng_get_node_as_of", map[string]string{
		"node_id": "node-99",
		"repo_id": "repo1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if resp.IncludedStaging {
		t.Error("expected IncludedStaging=false for eng_get_node_as_of")
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(resp.Nodes))
	}
	// Must return the promoted (KindFunction), not the staged (KindMethod).
	if resp.Nodes[0].Kind != domain.KindFunction {
		t.Errorf("expected promoted kind %q, got %q", domain.KindFunction, resp.Nodes[0].Kind)
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
