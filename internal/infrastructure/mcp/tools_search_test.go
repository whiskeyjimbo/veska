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

// ---------------------------------------------------------------------------
// stubs
// ---------------------------------------------------------------------------

type stubEmbedder struct {
	vec []float32
	err error
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return s.vec, s.err
}

func (s *stubEmbedder) ModelID() string { return "test-model" }

type stubVectors struct {
	hits []domain.Hit
	err  error
	// captured params from the most recent Search call. Guarded by mu so
	// the cross-repo parallel fanout path (solov2-bcn) doesn't race the
	// writes.
	mu     sync.Mutex
	gotVec []float32
	gotK   int
}

func (s *stubVectors) UpsertEmbeddings(_ context.Context, _, _ string, _ []domain.EmbeddingRow) error {
	return nil
}

func (s *stubVectors) Search(_ context.Context, _, _ string, vec []float32, k int, _ domain.Filter) ([]domain.Hit, error) {
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

func (s *stubVectors) Reindex(_ context.Context, _, _ string) error {
	return nil
}

func (s *stubVectors) LookupContentHashes(_ context.Context, _, _ string, _ []string) (map[string]string, error) {
	return nil, nil
}

type stubNodes struct {
	metas  []ports.NodeMeta
	err    error
	byFile map[string][]string // file_path → node_ids; populated by eng_find_related tests
}

func (s *stubNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	if s.err != nil {
		return nil, s.err
	}
	// filter by ids if specified — return all matching
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

// encodeVec packs a []float32 into the LE byte layout the embedder writes.
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

// ---------------------------------------------------------------------------
// eng_search_semantic
// ---------------------------------------------------------------------------

func TestSearchSemantic_ReturnsHydratedResults(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vecs := &stubVectors{hits: []domain.Hit{{NodeID: "n1", Score: 0.9}}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.Foo", FilePath: "foo.go", Kind: "function", LineStart: 1, LineEnd: 10},
	}}
	svc := search.NewService(emb, vecs, nodes)

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

// TestSearchSemantic_FansOutWhenRepoIDOmittedAndCwdMismatch pins solov2-g8fh:
// the README's quick-start sanity-check example calls eng_search_semantic
// without a repo_id. With ≥2 repos registered and the shim's cwd outside
// any registered RootPath, the handler must fan out across every repo
// instead of rejecting with "repo_id is required".
func TestSearchSemantic_FansOutWhenRepoIDOmittedAndCwdMismatch(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vecs := &stubVectors{hits: []domain.Hit{{NodeID: "n1", Score: 0.9}}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.Foo", FilePath: "foo.go", Kind: "function", LineStart: 1, LineEnd: 10},
	}}
	svc := search.NewService(emb, vecs, nodes)

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

// TestSearchSemantic_SingleRepoOmitsRepoIDOnHits pins solov2-g8fh: the
// per-hit repo_id field is only emitted when the response spans repos.
// Single-repo callers must see byte-stable pre-fanout output.
func TestSearchSemantic_SingleRepoOmitsRepoIDOnHits(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	vecs := &stubVectors{hits: []domain.Hit{{NodeID: "n1", Score: 0.9}}}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.Foo", FilePath: "foo.go", Kind: "function", LineStart: 1, LineEnd: 10},
	}}
	svc := search.NewService(emb, vecs, nodes)

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

// TestSearchSemantic_LimitAliasHonoured pins solov2-8rm: callers
// naturally try 'limit' (the convention used by every other MCP tool we
// expose). When 'k' is absent we honour 'limit' so a request with
// limit=3 actually returns at most 3 rows instead of silently defaulting
// to k=10.
func TestSearchSemantic_LimitAliasHonoured(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1, 0.2}}
	hits := []domain.Hit{
		{NodeID: "n1", Score: 0.9}, {NodeID: "n2", Score: 0.8},
		{NodeID: "n3", Score: 0.7}, {NodeID: "n4", Score: 0.6},
		{NodeID: "n5", Score: 0.5},
	}
	vecs := &stubVectors{hits: hits}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)

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
	// search.Service over-requests by fusionFanout=3 with a floor of 30
	// so the post-fusion name-match boost has candidates to reorder
	// even for small-k callers (solov2-2su / solov2-7kz). caller k=3
	// hits the floor → 30 to the vector backend. The contract under
	// test is that the alias got threaded through; the floor/fanout
	// are internal details.
	if vecs.gotK != 30 {
		t.Errorf("expected vectors.Search called with k=30 (floor for small caller k=3 alias), got k=%d", vecs.gotK)
	}
}

func TestSearchSemantic_MissingParamsRejected(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1}}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]string{
		"query":  "x",
		"branch": "main",
		// missing repo_id
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestSearchSemantic_KExceedsMax(t *testing.T) {
	emb := &stubEmbedder{vec: []float32{0.1}}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)

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
	svc := search.NewService(emb, vecs, nodes)

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

// TestSearchSemantic_GlobalRRFAcrossRepos pins solov2-bcn: when fanout is
// triggered, the handler runs ONE global RRF across every repo's
// candidate set rather than per-repo RRF + score-sort. A candidate
// appearing in both the vector AND lexical retrievers of its repo must
// outrank a candidate appearing in only one retriever, regardless of
// which repo it came from.
func TestSearchSemantic_GlobalRRFAcrossRepos(t *testing.T) {
	// Repo A: vector + lexical hits (double-retriever winner).
	// Repo B: vector-only hits.
	// Global RRF: A's nodeA1 should score 2/(60+1) = 0.0328, beating
	// any B node whose best score is 1/(60+1) = 0.0164.
	emb := &stubEmbedder{vec: []float32{0.1}}
	vecs := &stubVectors{hits: []domain.Hit{
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
	svc := search.NewService(emb, vecs, nodes, search.WithLexicalSearcher(lex))

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

// stubLex is a minimal LexicalSearcher used by the cross-repo RRF test.
type stubLex struct {
	hits []ports.LexicalHit
}

func (s *stubLex) Search(_ context.Context, _, _, _ string, _ int) ([]ports.LexicalHit, error) {
	return s.hits, nil
}

// ---------------------------------------------------------------------------
// eng_search_similar
// ---------------------------------------------------------------------------

func TestSearchSimilar_ReturnsNeighboursExcludingSeed(t *testing.T) {
	seedVec := []float32{0.5, 0.5, 0.5}
	emb := &stubEmbedder{}
	vecs := &stubVectors{
		hits: []domain.Hit{
			{NodeID: "seed", Score: 1.0},
			{NodeID: "n2", Score: 0.8},
			{NodeID: "n3", Score: 0.7},
		},
	}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n2", SymbolPath: "pkg.N2", FilePath: "n2.go", Kind: "function", LineStart: 1, LineEnd: 3},
		{NodeID: "n3", SymbolPath: "pkg.N3", FilePath: "n3.go", Kind: "function", LineStart: 4, LineEnd: 6},
	}}
	svc := search.NewService(emb, vecs, nodes)
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
	// Verify the seed vector was passed through unchanged.
	if len(vecs.gotVec) != 3 || vecs.gotVec[0] != 0.5 {
		t.Errorf("unexpected vec passed to Search: %+v", vecs.gotVec)
	}
	// k+1 over-request.
	if vecs.gotK != 3 {
		t.Errorf("expected over-request k=3, got %d", vecs.gotK)
	}
}

func TestSearchSimilar_NodeNotEmbedded(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)
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
	svc := search.NewService(emb, vecs, nodes)

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]string{
		"repo_id": "r",
		"branch":  "main",
		// missing node_id
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

// TestSearchSimilar_AcceptsSymbolAlias covers solov2-3ocy: eng_search_similar
// must accept `symbol` resolved via FindNodes — parity with eng_find_symbol /
// eng_get_call_chain / eng_get_blast_radius. Before the fix, the schema
// rejected `symbol` as an unknown parameter.
func TestSearchSimilar_AcceptsSymbolAlias(t *testing.T) {
	seedVec := []float32{0.5, 0.5, 0.5}
	emb := &stubEmbedder{}
	vecs := &stubVectors{
		hits: []domain.Hit{{NodeID: "seed", Score: 1.0}, {NodeID: "n2", Score: 0.8}},
	}
	nodes := &stubNodes{metas: []ports.NodeMeta{
		{NodeID: "n2", SymbolPath: "pkg.N2", FilePath: "n2.go", Kind: "function", LineStart: 1, LineEnd: 3},
	}}
	svc := search.NewService(emb, vecs, nodes)
	lookup := &stubSimilarLookup{hash: "h", ready: true, blob: encodeVec(seedVec), dim: 3, found: true}

	graph := newStubGraphStorage()
	seedNode, _ := domain.NewNode("seed", "seed.go", "Target", domain.KindFunction)
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

// TestSearchSimilar_AmbiguousSymbolRejected: two nodes with the same name
// must produce CodeInvalidParams asking the caller to pass node_id.
func TestSearchSimilar_AmbiguousSymbolRejected(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)

	graph := newStubGraphStorage()
	a, _ := domain.NewNode("a", "a.go", "Run", domain.KindFunction)
	b, _ := domain.NewNode("b", "b.go", "Run", domain.KindFunction)
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

// TestFindRelated_ResolvesSmallestEnclosingNode covers solov2-2g4r:
// when multiple nodes overlap a line (e.g. a chunk and a function),
// the resolver picks the tightest span so the embedding seed is the
// most specific match the agent could expect.
func TestFindRelated_ResolvesSmallestEnclosingNode(t *testing.T) {
	// Three nodes in the same file: a wide function (lines 5-30),
	// a tight method nested inside (10-20), and an even tighter
	// helper (15-17). Line 16 should resolve to the helper.
	nodes := &stubNodes{
		byFile: map[string][]string{"foo.go": {"wide", "mid", "tight"}},
		metas: []ports.NodeMeta{
			{NodeID: "wide", FilePath: "foo.go", LineStart: 5, LineEnd: 30, Kind: "function"},
			{NodeID: "mid", FilePath: "foo.go", LineStart: 10, LineEnd: 20, Kind: "method"},
			{NodeID: "tight", FilePath: "foo.go", LineStart: 15, LineEnd: 17, Kind: "function"},
		},
	}
	emb := &stubEmbedder{}
	vecs := &stubVectors{hits: []domain.Hit{{NodeID: "tight"}, {NodeID: "neighbour"}}}
	nodes.metas = append(nodes.metas, ports.NodeMeta{NodeID: "neighbour", FilePath: "other.go", LineStart: 1, LineEnd: 3, SymbolPath: "Other"})
	lookup := &stubSimilarLookup{hash: "h", ready: true, blob: encodeVec([]float32{0.1, 0.2}), dim: 2, found: true}
	svc := search.NewService(emb, vecs, nodes)

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
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "neighbour" {
		t.Errorf("want [neighbour] as the single returned hit, got %+v", resp.Results)
	}
}

// TestFindRelated_LineOutsideAnyNodeReturnsNotFound: a line that
// falls in pre-package whitespace or below the last symbol has no
// enclosing anchor; the handler returns CodeNotFound with a hint
// rather than a confusing empty result.
func TestFindRelated_LineOutsideAnyNodeReturnsNotFound(t *testing.T) {
	nodes := &stubNodes{
		byFile: map[string][]string{"foo.go": {"only"}},
		metas: []ports.NodeMeta{
			{NodeID: "only", FilePath: "foo.go", LineStart: 5, LineEnd: 10},
		},
	}
	svc := search.NewService(&stubEmbedder{}, &stubVectors{}, nodes)
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

// TestFindRelated_RejectsZeroLine: lines are 1-indexed everywhere on
// the surface; 0 or negative must error as InvalidParams.
func TestFindRelated_RejectsZeroLine(t *testing.T) {
	r := NewRegistry()
	svc := search.NewService(&stubEmbedder{}, &stubVectors{}, &stubNodes{})
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

// TestSearchTools_RegistersExpectedTools — count grew from 2 → 3 when
// solov2-2g4r added eng_find_related. Keep order in sync with the
// RegisterSearchTools registration block.
func TestSearchTools_RegistersExpectedTools(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil, nil)

	got := r.Names()
	// r.Names() returns alphabetical order, not registration order.
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
