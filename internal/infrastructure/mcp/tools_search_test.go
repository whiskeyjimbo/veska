// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

type stubEmbedder struct {
	vec []float32
	err error
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return s.vec, s.err
}

func (s *stubEmbedder) ModelID() string { return "test-model" }

type stubVectors struct {
	hits []domain.SearchHit
	err  error
	// gotVec and gotK are guarded by mu to prevent races when parallel fan-out search runs across multiple repositories.
	mu     sync.Mutex
	gotVec []float32
	gotK   int
}

func (s *stubVectors) UpsertEmbeddings(_ context.Context, _, _ string, _ []domain.EmbeddingRow) error {
	return nil
}

func (s *stubVectors) Search(_ context.Context, _, _ string, vec []float32, k int, _ domain.VectorFilter) ([]domain.SearchHit, error) {
	s.mu.Lock()
	s.gotVec = vec
	s.gotK = k
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if k < len(s.hits) {
		return s.hits[:k], nil
	}
	return s.hits, nil
}

func (s *stubVectors) DeleteNodes(context.Context, string, string, []string) error { return nil }
func (s *stubVectors) Reindex(_ context.Context, _, _ string) error {
	return nil
}

func (s *stubVectors) LookupContentHashes(_ context.Context, _, _ string, _ []string) (map[string]string, error) {
	return nil, nil
}

type stubNodes struct {
	metas  []ports.NodeMeta
	err    error
	byFile map[string][]string // file_path → node_ids
}

func (s *stubNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	if s.err != nil {
		return nil, s.err
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	var out []ports.NodeMeta
	for _, m := range s.metas {
		if _, ok := want[m.NodeID]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *stubNodes) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	if s.byFile == nil {
		return nil, nil
	}
	return s.byFile[filePath], nil
}

type stubSimilarLookup struct {
	hash    string
	ready   bool
	hashErr error

	blob     []byte
	dim      int
	found    bool
	existErr error
}

func (s *stubSimilarLookup) ContentHashForNode(_ context.Context, _, _, _ string) (string, bool, error) {
	return s.hash, s.ready, s.hashErr
}

func (s *stubSimilarLookup) LookupExisting(_ context.Context, _ string) ([]byte, int, bool, error) {
	return s.blob, s.dim, s.found, s.existErr
}

// encodeVec serializes a slice of float32s into a little-endian byte slice representation.
func encodeVec(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

func dispatchSearch(t *testing.T, r *Registry, method string, params any) (SearchResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: method, Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return SearchResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp SearchResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

func TestSearchSemantic_ReturnsHydratedResults(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vecs := &stubVectors{hits: []domain.SearchHit{{NodeID: "n1", Score: 0.9}}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.Foo", FilePath: "foo.go", Kind: "function", LineStart: 1, LineEnd: 10},
	}}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	resp, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query":   "find foo",
		"repo_id": "repo1",
		"branch":  "main",
		"k":       5,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "n1" {
		t.Fatalf("got %+v", resp.Results)
	}
	if resp.Results[0].Name != "pkg.Foo" {
		t.Errorf("expected hydrated name, got %q", resp.Results[0].Name)
	}
}

// TestSearchSemantic_FansOutWhenRepoIDOmittedAndCwdMismatch ensures semantic searches fan out across all registered repositories when the query is run without a repo ID outside any registered workspace root.
func TestSearchSemantic_FansOutWhenRepoIDOmittedAndCwdMismatch(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vecs := &stubVectors{hits: []domain.SearchHit{{NodeID: "n1", Score: 0.9}}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.Foo", FilePath: "foo.go", Kind: "function", LineStart: 1, LineEnd: 10},
	}}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/home/u/projects/alpha", ActiveBranch: "main"},
		{RepoID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootPath: "/home/u/projects/beta", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, &stubRepoLister{repos: repos})

	resp, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query": "find foo",
		"cwd":   "/tmp/somewhere/else",
	})
	if rpcErr != nil {
		t.Fatalf("expected fanout success, got %+v", rpcErr)
	}
	if len(resp.Results) == 0 {
		t.Fatalf("expected hits from fanout, got empty results")
	}
	for i, h := range resp.Results {
		if h.RepoID == "" {
			t.Errorf("results[%d] missing repo_id on fanout response: %+v", i, h)
		}
	}
}

// TestSearchSemantic_SingleRepoOmitsRepoIDOnHits ensures the repo_id field is omitted from hits in a single-repository response.
func TestSearchSemantic_SingleRepoOmitsRepoIDOnHits(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vecs := &stubVectors{hits: []domain.SearchHit{{NodeID: "n1", Score: 0.9}}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.Foo", FilePath: "foo.go", Kind: "function", LineStart: 1, LineEnd: 10},
	}}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/abs/repo", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, &stubRepoLister{repos: repos})

	resp, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{"query": "find foo"})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(resp.Results))
	}
	if resp.Results[0].RepoID != "" {
		t.Errorf("single-repo response leaked repo_id=%q", resp.Results[0].RepoID)
	}
}

// TestSearchSemantic_LimitAliasHonored ensures that the limit parameter is respected as an alias for k.
func TestSearchSemantic_LimitAliasHonored(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	hits := []domain.SearchHit{
		{NodeID: "n1", Score: 0.9},
		{NodeID: "n2", Score: 0.8},
		{NodeID: "n3", Score: 0.7},
		{NodeID: "n4", Score: 0.6},
		{NodeID: "n5", Score: 0.5},
	}
	vecs := &stubVectors{hits: hits}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query":   "x",
		"repo_id": "repo1",
		"branch":  "main",
		"limit":   3,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	// search.Service over-requests to ensure enough candidates are retrieved for reordering, regardless of small caller k values.
	if vecs.gotK != 100 {
		t.Errorf("expected vectors.Search called with k=100 (floor for small caller k=3 alias), got k=%d", vecs.gotK)
	}
}

func TestSearchSemantic_MissingParamsRejected(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1}}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]string{
		"query":  "x",
		"branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestSearchSemantic_KExceedsMax(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1}}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query":   "x",
		"repo_id": "r",
		"branch":  "main",
		"k":       maxSearchK + 1,
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams for oversized k, got %+v", rpcErr)
	}
}

func TestSearchSemantic_PropagatesEmbedError(t *testing.T) {
	emb := &stubEmbedder{err: errors.New("boom")}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query":   "x",
		"repo_id": "r",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}

// TestSearchSemantic_GlobalRRFAcrossRepos ensures that a single global Reciprocal Rank Fusion runs across all candidates retrieved from fanned-out repositories.
func TestSearchSemantic_GlobalRRFAcrossRepos(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1}}
	vecs := &stubVectors{hits: []domain.SearchHit{
		{NodeID: "nodeA1"}, {NodeID: "nodeA2"},
	}}
	lex := &stubLex{hits: []ports.LexicalHit{
		{NodeID: "nodeA1"},
	}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "nodeA1", SymbolPath: "a.A1", FilePath: "a.go"},
		{NodeID: "nodeA2", SymbolPath: "a.A2", FilePath: "a.go"},
		{NodeID: "nodeB1", SymbolPath: "b.B1", FilePath: "b.go"},
	}}
	svc, err := search.NewService(emb, vecs, nodes, search.WithLexicalSearcher(lex))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	repos := []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: "/r/a", ActiveBranch: "main"},
		{RepoID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootPath: "/r/b", ActiveBranch: "main"},
	}
	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, &stubRepoLister{repos: repos})

	resp, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query": "x",
		"cwd":   "/elsewhere",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected non-empty results")
	}
	// nodeA1 (vector + lexical) must outrank nodeA2 (vector-only).
	if resp.Results[0].NodeID != "nodeA1" {
		t.Errorf("expected nodeA1 first (double-retriever winner); got %q", resp.Results[0].NodeID)
	}
	// All hits must carry repo_id when fanout fires.
	for i, h := range resp.Results {
		if h.RepoID == "" {
			t.Errorf("results[%d] missing repo_id: %+v", i, h)
		}
	}
}

// stubLex is a stub implementation of LexicalSearcher for reciprocal rank fusion tests.
type stubLex struct {
	hits []ports.LexicalHit
}

func (s *stubLex) Search(_ context.Context, _, _, _ string, _ int) ([]ports.LexicalHit, error) {
	return s.hits, nil
}

func TestSearchSimilar_ReturnsNeighborsExcludingSeed(t *testing.T) {
	seedVec := []float32{0.5, 0.5, 0.5}
	emb := &stubEmbedder{}
	vecs := &stubVectors{
		hits: []domain.SearchHit{
			{NodeID: "seed", Score: 1.0},
			{NodeID: "n2", Score: 0.8},
			{NodeID: "n3", Score: 0.7},
		},
	}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n2", SymbolPath: "pkg.N2", FilePath: "n2.go", Kind: "function", LineStart: 1, LineEnd: 3},
		{NodeID: "n3", SymbolPath: "pkg.N3", FilePath: "n3.go", Kind: "function", LineStart: 4, LineEnd: 6},
	}}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	lookup := &stubSimilarLookup{
		hash:  "h1",
		ready: true,
		blob:  encodeVec(seedVec),
		dim:   3,
		found: true,
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil, nil)

	resp, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]any{
		"node_id": "seed",
		"repo_id": "r1",
		"branch":  "main",
		"k":       2,
	})
	if rpcErr != nil {
		t.Fatalf("unexpected error: %+v", rpcErr)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results (seed filtered), got %d (%+v)", len(resp.Results), resp.Results)
	}
	if resp.Results[0].NodeID != "n2" || resp.Results[1].NodeID != "n3" {
		t.Errorf("unexpected order: %+v", resp.Results)
	}
	if len(vecs.gotVec) != 3 || vecs.gotVec[0] != 0.5 {
		t.Errorf("unexpected vec passed to Search: %+v", vecs.gotVec)
	}
	if vecs.gotK != 3 {
		t.Errorf("expected over-request k=3, got %d", vecs.gotK)
	}
}

func TestSearchSimilar_NodeNotEmbedded(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	lookup := &stubSimilarLookup{ready: false}

	r := NewRegistry()
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]any{
		"node_id": "n",
		"repo_id": "r",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %+v", rpcErr)
	}
}

func TestSearchSimilar_MissingParams(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]string{
		"repo_id": "r",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

// TestSearchSimilar_AcceptsSymbolAlias verifies that eng_search_similar accepts the symbol parameter and resolves it using a search graph.
func TestSearchSimilar_AcceptsSymbolAlias(t *testing.T) {
	seedVec := []float32{0.5, 0.5, 0.5}
	emb := &stubEmbedder{}
	vecs := &stubVectors{
		hits: []domain.SearchHit{{NodeID: "seed", Score: 1.0}, {NodeID: "n2", Score: 0.8}},
	}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n2", SymbolPath: "pkg.N2", FilePath: "n2.go", Kind: "function", LineStart: 1, LineEnd: 3},
	}}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	lookup := &stubSimilarLookup{hash: "h", ready: true, blob: encodeVec(seedVec), dim: 3, found: true}

	graph := newStubGraphStorage()
	seedNode, _ := domain.NewNode(domain.NodeSpec{ID: "seed", Path: "seed.go", Name: "Target", Kind: domain.KindFunction})
	graph.addNode(seedNode)

	r := NewRegistry()
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil, nil, WithSearchGraph(graph))

	resp, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]any{
		"symbol":  "Target",
		"repo_id": "r1",
		"branch":  "main",
		"k":       1,
	})
	if rpcErr != nil {
		t.Fatalf("symbol alias rejected: %+v", rpcErr)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "n2" {
		t.Errorf("expected [n2], got %+v", resp.Results)
	}
}

// TestSearchSimilar_AmbiguousSymbolRejected ensures that querying an ambiguous symbol name results in an invalid params error.
func TestSearchSimilar_AmbiguousSymbolRejected(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	graph := newStubGraphStorage()
	a, _ := domain.NewNode(domain.NodeSpec{ID: "a", Path: "a.go", Name: "Run", Kind: domain.KindFunction})
	b, _ := domain.NewNode(domain.NodeSpec{ID: "b", Path: "b.go", Name: "Run", Kind: domain.KindFunction})
	graph.addNode(a)
	graph.addNode(b)

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil, WithSearchGraph(graph))

	_, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]any{
		"symbol": "Run", "repo_id": "r1", "branch": "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for ambiguous symbol, got %+v", rpcErr)
	}
}

// TestSearchSimilar_SymbolResolvesCrossRepoWithoutRepoID verifies that a symbol-based search can run across repositories without a repo ID, using a single-repo short-circuit.
func TestSearchSimilar_SymbolResolvesCrossRepoWithoutRepoID(t *testing.T) {
	seedVec := []float32{0.5, 0.5, 0.5}
	emb := &stubEmbedder{}
	vecs := &stubVectors{
		hits: []domain.SearchHit{{NodeID: "seed", Score: 1.0}, {NodeID: "n2", Score: 0.8}},
	}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n2", SymbolPath: "pkg.N2", FilePath: "n2.go", Kind: "function", LineStart: 1, LineEnd: 3},
	}}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	lookup := &stubSimilarLookup{hash: "h", ready: true, blob: encodeVec(seedVec), dim: 3, found: true}

	graph := newStubGraphStorage()
	seedNode, _ := domain.NewNode(domain.NodeSpec{ID: "seed", Path: "seed.go", Name: "Target", Kind: domain.KindFunction})
	graph.addNode(seedNode)
	repos := []application.RepoRecord{
		{RepoID: "r1", RootPath: "/r1", ActiveBranch: "main"},
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil, &stubRepoLister{repos: repos}, WithSearchGraph(graph))

	resp, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]any{
		"symbol": "Target",
		"k":      1,
		// no repo_id, no branch - must resolve via the single-repo
		// short-circuit in resolveSeedOwner.
	})
	if rpcErr != nil {
		t.Fatalf("symbol-only call rejected: %+v", rpcErr)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "n2" {
		t.Errorf("expected [n2], got %+v", resp.Results)
	}
}

// TestFindRelated_ResolvesSmallestEnclosingNode ensures that the resolved enclosing node is the most specific (smallest span) overlap when multiple node ranges match the requested line.
func TestFindRelated_ResolvesSmallestEnclosingNode(t *testing.T) {
	nodes := &stubNodes{
		byFile: map[string][]string{"foo.go": {"wide", "mid", "tight"}},
		metas: []ports.NodeMeta{
			{NodeID: "wide", FilePath: "foo.go", LineStart: 5, LineEnd: 30, Kind: "function"},
			{NodeID: "mid", FilePath: "foo.go", LineStart: 10, LineEnd: 20, Kind: "method"},
			{NodeID: "tight", FilePath: "foo.go", LineStart: 15, LineEnd: 17, Kind: "function"},
		},
	}
	emb := &stubEmbedder{}
	vecs := &stubVectors{hits: []domain.SearchHit{{NodeID: "tight"}, {NodeID: "neighbor"}}}
	nodes.metas = append(nodes.metas, ports.NodeMeta{NodeID: "neighbor", FilePath: "other.go", LineStart: 1, LineEnd: 3, SymbolPath: "Other"})
	lookup := &stubSimilarLookup{hash: "h", ready: true, blob: encodeVec([]float32{0.1, 0.2}), dim: 2, found: true}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil, nil)

	resp, rpcErr := dispatchSearch(t, r, "eng_find_related", map[string]any{
		"file_path": "foo.go",
		"line":      16,
		"repo_id":   "r1",
		"branch":    "main",
		"k":         1,
	})
	if rpcErr != nil {
		t.Fatalf("dispatch: %+v", rpcErr)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "neighbor" {
		t.Errorf("want [neighbor] as the single returned hit, got %+v", resp.Results)
	}
}

// TestFindRelated_ResolvesRelativePath ensures that relative and absolute file paths are correctly resolved against the repository root.
func TestFindRelated_ResolvesRelativePath(t *testing.T) {
	const root = "/abs/repo"
	nodes := &stubNodes{
		byFile: map[string][]string{"foo.go": {"seed"}},
		metas: []ports.NodeMeta{
			{NodeID: "seed", FilePath: "foo.go", LineStart: 5, LineEnd: 30, Kind: "function"},
			{NodeID: "neighbor", FilePath: "other.go", LineStart: 1, LineEnd: 3, SymbolPath: "Other"},
		},
	}
	vecs := &stubVectors{hits: []domain.SearchHit{{NodeID: "seed"}, {NodeID: "neighbor"}}}
	lookup := &stubSimilarLookup{hash: "h", ready: true, blob: encodeVec([]float32{0.1, 0.2}), dim: 2, found: true}
	svc, err := search.NewService(&stubEmbedder{}, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	repos := &stubRepoLister{repos: []application.RepoRecord{
		{RepoID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootPath: root, ActiveBranch: "main"},
	}}
	r := NewRegistry()
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil, repos)

	resp, rpcErr := dispatchSearch(t, r, "eng_find_related", map[string]any{
		"file_path": "foo.go",
		"line":      16,
		"repo_id":   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"branch":    "main",
		"k":         1,
	})
	if rpcErr != nil {
		t.Fatalf("relative path should resolve, got %+v", rpcErr)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "neighbor" {
		t.Errorf("want [neighbor] from resolved relative path, got %+v", resp.Results)
	}

	respAbs, rpcErr := dispatchSearch(t, r, "eng_find_related", map[string]any{
		"file_path": root + "/foo.go",
		"line":      16,
		"repo_id":   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"branch":    "main",
		"k":         1,
	})
	if rpcErr != nil {
		t.Fatalf("absolute path should still work, got %+v", rpcErr)
	}
	if len(respAbs.Results) != 1 || respAbs.Results[0].NodeID != "neighbor" {
		t.Errorf("want [neighbor] from absolute path, got %+v", respAbs.Results)
	}
}

// TestFindRelated_LineOutsideAnyNodeReturnsNotFound ensures that a line request falling outside any defined node span returns a CodeNotFound error.
func TestFindRelated_LineOutsideAnyNodeReturnsNotFound(t *testing.T) {
	nodes := &stubNodes{
		byFile: map[string][]string{"foo.go": {"only"}},
		metas: []ports.NodeMeta{
			{NodeID: "only", FilePath: "foo.go", LineStart: 5, LineEnd: 10},
		},
	}
	svc, err := search.NewService(&stubEmbedder{}, &stubVectors{}, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, &stubVectors{}, nodes, nil, nil)
	_, rpcErr := dispatchSearch(t, r, "eng_find_related", map[string]any{
		"file_path": "foo.go",
		"line":      99,
		"repo_id":   "r1",
		"branch":    "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("want CodeNotFound for line outside ranges, got %+v", rpcErr)
	}
}

// TestFindRelated_RejectsZeroLine ensures that requests for line 0 are rejected with an invalid parameters error.
func TestFindRelated_RejectsZeroLine(t *testing.T) {
	r := NewRegistry()
	svc, err := search.NewService(&stubEmbedder{}, &stubVectors{}, &stubNodes{})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, &stubVectors{}, &stubNodes{}, nil, nil)
	_, rpcErr := dispatchSearch(t, r, "eng_find_related", map[string]any{
		"file_path": "foo.go",
		"line":      0,
		"repo_id":   "r1",
		"branch":    "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams for line=0, got %+v", rpcErr)
	}
}

// TestSearchTools_RegistersExpectedTools ensures all search tools are registered correctly in alphabetical order.
func TestSearchTools_RegistersExpectedTools(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc, err := search.NewService(emb, vecs, nodes)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	got := r.Names()
	// r.Names returns alphabetical order, not registration order.
	want := []string{"eng_find_related", "eng_search_semantic", "eng_search_similar"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tools, got %d (%v)", len(want), len(got), got)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("expected %q at %d, got %q", n, i, got[i])
		}
	}
}
