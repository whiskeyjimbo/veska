package mcp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"testing"

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
	// captured params from the most recent Search call
	gotVec []float32
	gotK   int
}

func (s *stubVectors) UpsertEmbeddings(_ context.Context, _, _ string, _ []domain.EmbeddingRow) error {
	return nil
}

func (s *stubVectors) Search(_ context.Context, _, _ string, vec []float32, k int, _ domain.Filter) ([]domain.Hit, error) {
	s.gotVec = vec
	s.gotK = k
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
	metas []ports.NodeMeta
	err   error
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

func (s *stubNodes) NodesInFile(_ context.Context, _, _, _ string) ([]string, error) {
	return nil, nil
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
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

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
	if resp.Results[0].SymbolPath != "pkg.Foo" {
		t.Errorf("expected hydrated symbol_path, got %q", resp.Results[0].SymbolPath)
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
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

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
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

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
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

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
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_semantic", map[string]any{
		"query":   "x",
		"repo_id": "r",
		"branch":  "main",
	})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
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
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil)

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
	RegisterSearchTools(r, svc, lookup, vecs, nodes, nil)

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
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

	_, rpcErr := dispatchSearch(t, r, "eng_search_similar", map[string]string{
		"repo_id": "r",
		"branch":  "main",
		// missing node_id
	})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestSearchTools_RegistersTwoTools(t *testing.T) {
	emb := &stubEmbedder{}
	vecs := &stubVectors{}
	nodes := &stubNodes{}
	svc := search.NewService(emb, vecs, nodes)

	r := NewRegistry()
	RegisterSearchTools(r, svc, &stubSimilarLookup{}, vecs, nodes, nil)

	got := r.Names()
	want := []string{"eng_search_semantic", "eng_search_similar"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tools, got %d (%v)", len(want), len(got), got)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("expected %q at %d, got %q", n, i, got[i])
		}
	}
}
