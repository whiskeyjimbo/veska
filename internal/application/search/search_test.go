package search_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// --- fakes -----------------------------------------------------------------

type fakeEmbedder struct {
	vec     []float32
	err     error
	calls   int
	gotText string
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	f.gotText = text
	return f.vec, f.err
}
func (f *fakeEmbedder) ModelID() string { return "test-model" }

type fakeVectors struct {
	hits      []domain.Hit
	err       error
	calls     int
	gotK      int
	gotVec    []float32
	gotRepo   string
	gotBranch string
}

func (f *fakeVectors) UpsertEmbeddings(_ context.Context, _, _ string, _ []domain.EmbeddingRow) error {
	return nil
}
func (f *fakeVectors) Search(_ context.Context, repoID, branch string, vec []float32, k int, _ domain.Filter) ([]domain.Hit, error) {
	f.calls++
	f.gotRepo = repoID
	f.gotBranch = branch
	f.gotVec = vec
	f.gotK = k
	return f.hits, f.err
}
func (f *fakeVectors) Reindex(_ context.Context, _, _ string) error { return nil }
func (f *fakeVectors) LookupContentHashes(_ context.Context, _, _ string, _ []string) (map[string]string, error) {
	return nil, nil
}

type fakeNodes struct {
	rows   []ports.NodeMeta
	err    error
	calls  int
	gotIDs []string
}

func (f *fakeNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	f.calls++
	f.gotIDs = append([]string(nil), ids...)
	return f.rows, f.err
}

// NodesInFile is required by ports.NodeLookup. The search service does not
// call it, so the fake's behaviour here is irrelevant to the tests; we keep
// it returning nil to satisfy the interface.
func (f *fakeNodes) NodesInFile(_ context.Context, _, _, _ string) ([]string, error) {
	return nil, nil
}

// --- tests -----------------------------------------------------------------

// TestSemantic_HappyPath_PreservesHitRank verifies the service returns
// hydrated Results in the order VectorStorage.Search produced — even
// when the NodeLookup adapter returns rows in a different order.
func TestSemantic_HappyPath_PreservesHitRank(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{vec: []float32{0.1, 0.2}}
	vec := &fakeVectors{hits: []domain.Hit{
		{NodeID: "n2", Score: 0.99},
		{NodeID: "n1", Score: 0.80},
		{NodeID: "n3", Score: 0.70},
	}}
	// Deliberately return in a different order than hits.
	nodes := &fakeNodes{rows: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.A", FilePath: "a.go", Kind: "function", LineStart: 1, LineEnd: 10},
		{NodeID: "n3", SymbolPath: "pkg.C", FilePath: "c.go", Kind: "type", LineStart: 30, LineEnd: 40},
		{NodeID: "n2", SymbolPath: "pkg.B", FilePath: "b.go", Kind: "method", LineStart: 20, LineEnd: 25},
	}}

	s := search.NewService(emb, vec, nodes)
	resp, err := s.Semantic(context.Background(), "r1", "main", "find foo", 10, domain.Filter{})
	if err != nil {
		t.Fatalf("Semantic: %v", err)
	}
	got := resp.Results
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	if len(resp.DegradedReasons) != 0 {
		t.Errorf("happy path must not set degraded_reasons, got %v", resp.DegradedReasons)
	}
	want := []string{"n2", "n1", "n3"}
	for i, r := range got {
		if r.NodeID != want[i] {
			t.Errorf("rank %d: got %q want %q", i, r.NodeID, want[i])
		}
	}
	if got[0].SymbolPath != "pkg.B" || got[0].Score != 0.99 {
		t.Errorf("top hit not hydrated correctly: %+v", got[0])
	}
	if emb.gotText != "find foo" {
		t.Errorf("embedder text = %q want %q", emb.gotText, "find foo")
	}
	if vec.gotK != 10 || vec.gotRepo != "r1" || vec.gotBranch != "main" {
		t.Errorf("vectors got repo=%q branch=%q k=%d", vec.gotRepo, vec.gotBranch, vec.gotK)
	}
}

// TestSemantic_MissingNodesDroppedSilently verifies a hit whose node row
// is absent from NodeLookup is omitted from the result without error.
func TestSemantic_MissingNodesDroppedSilently(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{vec: []float32{1}}
	vec := &fakeVectors{hits: []domain.Hit{
		{NodeID: "alive", Score: 0.9},
		{NodeID: "dangling", Score: 0.8},
		{NodeID: "also-alive", Score: 0.7},
	}}
	nodes := &fakeNodes{rows: []ports.NodeMeta{
		{NodeID: "alive", SymbolPath: "pkg.A", FilePath: "a.go", Kind: "function"},
		{NodeID: "also-alive", SymbolPath: "pkg.B", FilePath: "b.go", Kind: "function"},
	}}

	s := search.NewService(emb, vec, nodes)
	resp, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err != nil {
		t.Fatalf("Semantic: %v", err)
	}
	got := resp.Results
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(got), got)
	}
	if got[0].NodeID != "alive" || got[1].NodeID != "also-alive" {
		t.Errorf("rank wrong: %+v", got)
	}
}

// TestSemantic_EmbedderError_PropagatesWrapped verifies embedder errors
// surface to the caller wrapped so they're identifiable upstream.
func TestSemantic_EmbedderError_PropagatesWrapped(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("embed boom")
	emb := &fakeEmbedder{err: sentinel}
	vec := &fakeVectors{}
	nodes := &fakeNodes{}

	s := search.NewService(emb, vec, nodes)
	_, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err == nil {
		t.Fatal("expected error")
		return
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error not wrapped: %v", err)
	}
	if vec.calls != 0 || nodes.calls != 0 {
		t.Errorf("downstream calls happened after embed failure: vec=%d nodes=%d", vec.calls, nodes.calls)
	}
}

// TestSemantic_VectorStorageError_PropagatesWrapped verifies VectorStorage
// errors surface wrapped and NodeLookup is not invoked.
func TestSemantic_VectorStorageError_PropagatesWrapped(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("vec boom")
	emb := &fakeEmbedder{vec: []float32{1}}
	vec := &fakeVectors{err: sentinel}
	nodes := &fakeNodes{}

	s := search.NewService(emb, vec, nodes)
	_, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err == nil {
		t.Fatal("expected error")
		return
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error not wrapped: %v", err)
	}
	if nodes.calls != 0 {
		t.Errorf("nodes should not be called on vector error, calls=%d", nodes.calls)
	}
}

// TestSemantic_EmptyHits_ReturnsEmptyNilError verifies a zero-hit search
// short-circuits before hitting NodeLookup.
func TestSemantic_EmptyHits_ReturnsEmptyNilError(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{vec: []float32{1}}
	vec := &fakeVectors{hits: nil}
	nodes := &fakeNodes{}

	s := search.NewService(emb, vec, nodes)
	resp, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err != nil {
		t.Fatalf("Semantic: %v", err)
	}
	got := resp.Results
	if got == nil {
		t.Fatal("expected empty slice, got nil — callers serialize nil as null")
		return
	}
	if len(got) != 0 {
		t.Errorf("expected 0 results, got %d", len(got))
	}
	if nodes.calls != 0 {
		t.Errorf("nodes should not be called on empty hits, calls=%d", nodes.calls)
	}
}

// TestSemantic_KZero_ShortCircuits verifies k<=0 returns immediately
// without invoking embedder or vectors.
func TestSemantic_KZero_ShortCircuits(t *testing.T) {
	t.Parallel()
	for _, k := range []int{0, -1, -100} {
		emb := &fakeEmbedder{}
		vec := &fakeVectors{}
		nodes := &fakeNodes{}
		s := search.NewService(emb, vec, nodes)

		resp, err := s.Semantic(context.Background(), "r1", "main", "q", k, domain.Filter{})
		if err != nil {
			t.Fatalf("k=%d: %v", k, err)
		}
		if len(resp.Results) != 0 {
			t.Errorf("k=%d: expected 0 results, got %d", k, len(resp.Results))
		}
		if emb.calls != 0 || vec.calls != 0 || nodes.calls != 0 {
			t.Errorf("k=%d: dependencies invoked (emb=%d vec=%d nodes=%d)", k, emb.calls, vec.calls, nodes.calls)
		}
	}
}

// TestSemantic_NodeLookupError_PropagatesWrapped verifies a NodeLookup
// failure surfaces wrapped.
func TestSemantic_NodeLookupError_PropagatesWrapped(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lookup boom")
	emb := &fakeEmbedder{vec: []float32{1}}
	vec := &fakeVectors{hits: []domain.Hit{{NodeID: "n1", Score: 1}}}
	nodes := &fakeNodes{err: sentinel}

	s := search.NewService(emb, vec, nodes)
	_, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err == nil {
		t.Fatal("expected error")
		return
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error not wrapped: %v", err)
	}
}

// TestSemantic_ObservesVectorQueryDuration_HappyPath verifies the metric
// histogram observes exactly one sample per call.
func TestSemantic_ObservesVectorQueryDuration_HappyPath(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	emb := &fakeEmbedder{vec: []float32{1}}
	vec := &fakeVectors{hits: []domain.Hit{{NodeID: "n1", Score: 1}}}
	nodes := &fakeNodes{rows: []ports.NodeMeta{{NodeID: "n1", SymbolPath: "p.A", FilePath: "a.go", Kind: "function"}}}

	s := search.NewService(emb, vec, nodes, search.WithMetrics(m))
	if _, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{}); err != nil {
		t.Fatalf("Semantic: %v", err)
	}

	n := testutil.CollectAndCount(m.VectorQueryDuration)
	if n < 1 {
		t.Errorf("expected at least one VectorQueryDuration series after Semantic, got %d", n)
	}
}

// TestSemantic_ObservesVectorQueryDuration_ErrorPath verifies the metric
// is observed even when the call ultimately errors.
func TestSemantic_ObservesVectorQueryDuration_ErrorPath(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	emb := &fakeEmbedder{err: errors.New("boom")}
	vec := &fakeVectors{}
	nodes := &fakeNodes{}

	s := search.NewService(emb, vec, nodes, search.WithMetrics(m))
	if _, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{}); err == nil {
		t.Fatal("expected error")
		return
	}

	n := testutil.CollectAndCount(m.VectorQueryDuration)
	if n < 1 {
		t.Errorf("expected at least one VectorQueryDuration series after error, got %d", n)
	}
}

// --- lexical fallback (m3.03.2) -------------------------------------------

type fakeLexical struct {
	hits   []ports.LexicalHit
	err    error
	calls  int
	gotQ   string
	gotK   int
	gotRep string
	gotBr  string
}

func (f *fakeLexical) Search(_ context.Context, repoID, branch, query string, k int) ([]ports.LexicalHit, error) {
	f.calls++
	f.gotRep = repoID
	f.gotBr = branch
	f.gotQ = query
	f.gotK = k
	return f.hits, f.err
}

// TestSemantic_EmbedderUnreachable_FallsBackToLexical verifies the
// ErrEmbedderUnreachable sentinel triggers the lexical arm and the
// envelope carries the canonical degraded_reasons token.
func TestSemantic_EmbedderUnreachable_FallsBackToLexical(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{err: fmt.Errorf("dial: %w", ports.ErrEmbedderUnreachable)}
	vec := &fakeVectors{}
	nodes := &fakeNodes{rows: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.A", FilePath: "a.go", Kind: "function", LineStart: 1, LineEnd: 5},
	}}
	lex := &fakeLexical{hits: []ports.LexicalHit{{NodeID: "n1", Score: 0.5}}}

	s := search.NewService(emb, vec, nodes, search.WithLexicalSearcher(lex))
	resp, err := s.Semantic(context.Background(), "r1", "main", "close", 5, domain.Filter{})
	if err != nil {
		t.Fatalf("Semantic with unreachable embedder + lexical: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "n1" {
		t.Errorf("expected lexical hit n1 hydrated, got %+v", resp.Results)
	}
	if len(resp.DegradedReasons) != 1 ||
		resp.DegradedReasons[0] != search.DegradedReasonEmbedderOfflineLexicalFallback {
		t.Errorf("degraded_reasons = %v, want [%s]",
			resp.DegradedReasons, search.DegradedReasonEmbedderOfflineLexicalFallback)
	}
	if vec.calls != 0 {
		t.Errorf("vectors must not be invoked on fallback, calls=%d", vec.calls)
	}
	if lex.gotQ != "close" || lex.gotK != 5 || lex.gotRep != "r1" || lex.gotBr != "main" {
		t.Errorf("lexical args: q=%q k=%d repo=%q branch=%q", lex.gotQ, lex.gotK, lex.gotRep, lex.gotBr)
	}
}

// TestSemantic_EmbedderUnreachable_NoLexical_PropagatesError verifies
// that without a LexicalSearcher wired in, ErrEmbedderUnreachable
// surfaces wrapped to the caller — no silent zero-result return.
func TestSemantic_EmbedderUnreachable_NoLexical_PropagatesError(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{err: fmt.Errorf("dial: %w", ports.ErrEmbedderUnreachable)}
	vec := &fakeVectors{}
	nodes := &fakeNodes{}

	s := search.NewService(emb, vec, nodes) // no WithLexicalSearcher
	_, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err == nil {
		t.Fatal("expected error, got nil")
		return
	}
	if !errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Errorf("expected ErrEmbedderUnreachable in chain, got %v", err)
	}
}

// TestSemantic_NonSentinelEmbedderError_DoesNotFallBack verifies that a
// generic embedder error (not ErrEmbedderUnreachable) propagates even
// when a LexicalSearcher is installed — fallback is restricted to the
// sentinel so genuinely actionable failures aren't masked.
func TestSemantic_NonSentinelEmbedderError_DoesNotFallBack(t *testing.T) {
	t.Parallel()
	other := errors.New("model bad input")
	emb := &fakeEmbedder{err: other}
	vec := &fakeVectors{}
	nodes := &fakeNodes{}
	lex := &fakeLexical{}

	s := search.NewService(emb, vec, nodes, search.WithLexicalSearcher(lex))
	_, err := s.Semantic(context.Background(), "r1", "main", "q", 5, domain.Filter{})
	if err == nil {
		t.Fatal("expected error, got nil")
		return
	}
	if !errors.Is(err, other) {
		t.Errorf("expected wrapped non-sentinel error, got %v", err)
	}
	if lex.calls != 0 {
		t.Errorf("lexical must not be invoked for non-sentinel embedder errors, calls=%d", lex.calls)
	}
}

// TestLexical_HappyPath verifies the explicit Lexical method runs the
// LexicalSearcher and hydrates via NodeLookup, no embedder calls.
func TestLexical_HappyPath(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{}
	vec := &fakeVectors{}
	nodes := &fakeNodes{rows: []ports.NodeMeta{
		{NodeID: "n1", SymbolPath: "pkg.A", FilePath: "a.go", Kind: "function"},
		{NodeID: "n2", SymbolPath: "pkg.B", FilePath: "b.go", Kind: "function"},
	}}
	lex := &fakeLexical{hits: []ports.LexicalHit{
		{NodeID: "n1", Score: 0.9},
		{NodeID: "n2", Score: 0.5},
	}}

	s := search.NewService(emb, vec, nodes, search.WithLexicalSearcher(lex))
	got, err := s.Lexical(context.Background(), "r1", "main", "close", 10)
	if err != nil {
		t.Fatalf("Lexical: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].NodeID != "n1" || got[1].NodeID != "n2" {
		t.Errorf("rank order lost: %+v", got)
	}
	if emb.calls != 0 || vec.calls != 0 {
		t.Errorf("Lexical must not call embedder/vectors, emb=%d vec=%d", emb.calls, vec.calls)
	}
}

// TestLexical_NoLexicalWired_ReturnsNil verifies Lexical short-circuits
// to nil when no LexicalSearcher option was applied.
func TestLexical_NoLexicalWired_ReturnsNil(t *testing.T) {
	t.Parallel()
	s := search.NewService(&fakeEmbedder{}, &fakeVectors{}, &fakeNodes{})
	got, err := s.Lexical(context.Background(), "r1", "main", "q", 5)
	if err != nil {
		t.Fatalf("Lexical: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result without lexical wired, got %+v", got)
	}
}

// TestNewService_PanicsOnNilDeps verifies the construction-time nil check
// short-circuits programmer errors at boot rather than at first query.
func TestNewService_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	emb := &fakeEmbedder{}
	vec := &fakeVectors{}
	nodes := &fakeNodes{}

	cases := []struct {
		name string
		e    ports.EmbeddingProvider
		v    ports.VectorStorage
		n    ports.NodeLookup
	}{
		{"nil embedder", nil, vec, nodes},
		{"nil vectors", emb, nil, nodes},
		{"nil nodes", emb, vec, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s", tc.name)
				}
			}()
			_ = search.NewService(tc.e, tc.v, tc.n)
		})
	}
}
