// Package search contains the application-layer semantic-search service.
//
// Scope (m3.03.1): backend-agnostic k-NN. The service embeds the query,
// dispatches the k-NN through ports.VectorStorage (which itself routes
// to the configured vector backend per ADR-S0015), then hydrates the
// returned node_ids into source-location-bearing Results via
// ports.NodeLookup.
//
// Out of scope: lexical fallback when the embedder is unreachable
// (m3.03.2), eval/recall harness (m3.03.3), MCP tool registration
// (m3.06.1), and degraded_reasons envelope construction (MCP layer).
package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// DegradedReasonEmbedderOfflineLexicalFallback is the canonical token
// emitted on Response.DegradedReasons when Semantic falls back to the
// lexical arm because the embedder was unreachable. The literal matches
// the string defined in SOLO-13 §4 and SOLO-09 §4.5 so MCP/HTTP envelopes
// can forward it unchanged.
const DegradedReasonEmbedderOfflineLexicalFallback = "embedder_offline_lexical_fallback"

// DegradedReasonLowQualityStaticEmbedder is emitted on every Semantic
// response when the elected embedder is the in-binary static-v2 fallback —
// a low-quality last resort used only when model2vec is unavailable. It
// surfaces the quality cliff in-band so an agent (or `veska search`) can
// tell the user to run `veska install model2vec` instead of silently
// trusting near-noise scores (solov2-d2x).
const DegradedReasonLowQualityStaticEmbedder = "low_quality_static_embedder"

// staticEmbedderModelID mirrors the static adapter's ModelID. It is a
// literal rather than an import because the application layer must not
// depend on an infrastructure adapter (enforced by `make layercheck`); the
// value is a stable wire identifier, and the matching test in the static
// package pins it so drift is caught.
const staticEmbedderModelID = "veska-static-v2"

// Result is a hydrated semantic-search hit. The Score is the raw value
// returned by VectorStorage.Search (distance-derived, higher = better);
// callers preserve the ordering imposed by VectorStorage.
type Result struct {
	NodeID     string
	Score      float32
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
	// Snippet is the symbol's source code, populated from the nodes
	// table's snippet column. Lets agents skip a follow-up Read of the
	// file (solov2-7kz). Empty when the underlying node has no stored
	// content (legacy rows from before the snippet column existed).
	Snippet string
}

// Response is the envelope returned by Semantic. It carries the hydrated
// Results plus any degraded_reasons that describe why the path the
// service actually took differed from the happy path (e.g. embedder
// offline → lexical fallback). The wrapper exists so callers can branch
// on degradation without inspecting errors, and so additional reasons
// (rate-limit, stale index, ...) can be added without breaking the
// signature.
type Response struct {
	Results         []Result
	DegradedReasons []string
}

// RankedCandidate is a single hydrated search candidate plus the
// per-retriever ranks it earned in its source repo. The MCP cross-repo
// fanout (solov2-bcn) uses this to RRF a globally pooled candidate set —
// without it, each repo's local RRF score is incomparable across repos
// (every repo has a rank-1 hit scoring ~1/61, so 'top hits' from a
// 5-repo workspace render as five equally-ranked items).
//
// VectorRank and LexicalRank are 1-indexed; 0 means the candidate was
// absent from that retriever. A candidate always appears in at least
// one of the two.
//
// VectorScore is the raw distance-derived score the VectorStorage
// returned (higher = better). It is comparable across queries against
// the same embedder, and — critically for cross-repo fanout — across
// repos when one embedder spans them (solov2-uuuk). 0 means the
// candidate didn't appear in the vector retriever, so cosine fusion
// must either fall back to a baseline contribution or drop it. The
// MCP cross-repo handler chooses cosine fusion over global RRF
// whenever non-zero VectorScores are present.
type RankedCandidate struct {
	Result
	VectorRank  int
	LexicalRank int
	VectorScore float32
}

// CandidatesResponse is the un-fused, hydrated cross-repo input shape
// returned by SemanticCandidates. Same DegradedReasons semantics as
// Response.
type CandidatesResponse struct {
	Candidates      []RankedCandidate
	DegradedReasons []string
}

// Service is the application-layer semantic-search orchestrator. It
// composes an EmbeddingProvider, a VectorStorage, and a NodeLookup —
// plus an optional LexicalSearcher used as the fallback path when the
// embedder is unreachable (m3.03.2). When no LexicalSearcher is wired
// in, embedder-unreachable errors propagate to the caller unchanged.
type Service struct {
	embedder ports.EmbeddingProvider
	vectors  ports.VectorStorage
	nodes    ports.NodeLookup
	lexical  ports.LexicalSearcher
	metrics  *observability.Metrics
	now      func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithMetrics installs a Metrics struct so the service can observe
// VectorQueryDuration{kind="semantic_search"} on every Semantic call.
// When nil, the histogram update is silently skipped — the service
// still functions.
func WithMetrics(m *observability.Metrics) Option {
	return func(s *Service) { s.metrics = m }
}

// WithLexicalSearcher installs a LexicalSearcher used as the fallback
// path when EmbeddingProvider.Embed returns ports.ErrEmbedderUnreachable.
// When nil, the fallback is disabled and embedder-unreachable errors
// propagate to the caller wrapped (alongside any other embedder error).
func WithLexicalSearcher(l ports.LexicalSearcher) Option {
	return func(s *Service) { s.lexical = l }
}

// WithClock overrides the time source used for VectorQueryDuration
// observations. Intended for deterministic tests; production callers
// should accept the default time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// NewService constructs a Service. Dependencies are required: a nil
// embedder, vectors, or nodes is a programmer error and is reported
// by panicking at construction time rather than failing later at the
// first query.
func NewService(embedder ports.EmbeddingProvider, vectors ports.VectorStorage, nodes ports.NodeLookup, opts ...Option) *Service {
	if embedder == nil {
		panic("search.NewService: embedder is nil")
	}
	if vectors == nil {
		panic("search.NewService: vectors is nil")
	}
	if nodes == nil {
		panic("search.NewService: nodes is nil")
	}
	s := &Service{
		embedder: embedder,
		vectors:  vectors,
		nodes:    nodes,
		now:      time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Semantic resolves query against the (repoID, branch) embedding index
// and returns up to k hydrated Results in VectorStorage rank order.
//
// k <= 0 short-circuits to an empty result without invoking the
// embedder or VectorStorage. An empty result from VectorStorage is
// returned as an empty slice with nil error. Hits whose backing node
// row is missing from the nodes table are silently dropped: the
// vector index is eventually-consistent vs SQL truth, and surfacing
// dangling hits would let the caller render a result with no source
// location.
//
// VectorQueryDuration{kind="semantic_search"} is observed once per
// call, including error paths (the duration is the time-to-error).
func (s *Service) Semantic(ctx context.Context, repoID, branch, query string, k int, filter domain.Filter) (Response, error) {
	if k <= 0 {
		return Response{}, nil
	}

	start := s.now()
	defer s.observe(start)

	vec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		// Only the ErrEmbedderUnreachable sentinel triggers fallback.
		// Every other embedder error — bad input, malformed config,
		// server-side 5xx that isn't a dial failure — surfaces wrapped
		// so the caller can decide. This narrow contract keeps the
		// fallback from masking genuinely actionable failures.
		if errors.Is(err, ports.ErrEmbedderUnreachable) && s.lexical != nil {
			results, lerr := s.lexicalFallback(ctx, repoID, branch, query, k)
			if lerr != nil {
				return Response{}, fmt.Errorf("search: lexical fallback after embedder unreachable: %w", lerr)
			}
			return s.withEmbedderCaveat(Response{
				Results:         results,
				DegradedReasons: []string{DegradedReasonEmbedderOfflineLexicalFallback},
			}), nil
		}
		return Response{}, fmt.Errorf("search: embed query: %w", err)
	}

	// Over-request from each retriever so RRF + the post-fusion
	// name-match boost have headroom — fusing two top-K lists where the
	// second-best in one only appears at rank K+1 in the other still
	// produces a sensible top-K. 3× is the sweet spot in the semble
	// paper. A minimum floor protects small-k callers so the rerank
	// (definition / stem / verb-synonym signals) has a meaningful pool
	// to draw from. solov2-izh6.26 widened the floor from 30 to 60 —
	// the canonical answer for "register subcommand" (cobra's
	// Command.AddCommand) sits at fused-rank ~22, so a floor of 30 left
	// it just outside the rerank window even though the synonym signal
	// would have promoted it to the top.
	const fusionFanout = 3
	const fanoutFloor = 100
	fanK := max(k*fusionFanout, fanoutFloor)
	vecHits, err := s.vectors.Search(ctx, repoID, branch, vec, fanK, filter)
	if err != nil {
		return Response{}, fmt.Errorf("search: vector search: %w", err)
	}

	// Hybrid: when a LexicalSearcher is wired, run BM25/FTS5 in parallel
	// and fuse with Reciprocal Rank Fusion (solov2-2su). Vector cosine
	// alone is too thin on small corpora — Sam's notes-API session
	// returned scores in a ~0.00004 range across the top-10 — so the
	// "right" answer routinely lost to neighbours by a rounding error.
	// RRF is rank-only so the two retrievers' incompatible score
	// distributions don't need normalising.
	//
	// When lexical is absent (no FTS5 wired, or empty corpus on that
	// side), the fusion path degrades to pure vector ordering.
	var lexHits []ports.LexicalHit
	if s.lexical != nil {
		lh, lerr := s.lexical.Search(ctx, repoID, branch, query, fanK)
		if lerr == nil {
			lexHits = lh
		}
		// A lexical-side failure is non-fatal: degrade to vector-only.
	}
	// rrfFuse keeps the FULL candidate pool so the post-fusion name-match
	// boost has room to reorder (otherwise a high-name-match candidate
	// sitting at fused rank k+1 gets truncated before it can win). Final
	// truncation to caller-k happens after the boost.
	hits := rrfFuse(vecHits, lexHits, 0)
	if len(hits) == 0 {
		return s.withEmbedderCaveat(Response{Results: []Result{}}), nil
	}

	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.NodeID
	}

	metas, err := s.nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return Response{}, fmt.Errorf("search: node lookup: %w", err)
	}

	// Index hydrated rows by node_id so we can stitch them back into
	// the rank order returned by VectorStorage. Missing rows fall
	// through as silent drops.
	byID := make(map[string]ports.NodeMeta, len(metas))
	for _, m := range metas {
		byID[m.NodeID] = m
	}

	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		m, ok := byID[h.NodeID]
		if !ok {
			continue
		}
		out = append(out, Result{
			NodeID:     h.NodeID,
			Score:      h.Score,
			SymbolPath: m.SymbolPath,
			FilePath:   m.FilePath,
			Kind:       m.Kind,
			LineStart:  m.LineStart,
			LineEnd:    m.LineEnd,
			Snippet:    m.Snippet,
		})
	}
	// Post-fusion reranking (solov2-2sf): definition boost, identifier
	// stems, file coherence, noise penalty. Subsumes the earlier
	// name-match boost (solov2-x35). All signals scale by the candidate
	// set's maxScore so they bite on tight-clustered small-corpus
	// distributions yet stay sub-noise on real corpora.
	out = rerank(out, query)
	if len(out) > k {
		out = out[:k]
	}
	return s.withEmbedderCaveat(Response{Results: out}), nil
}

// withEmbedderCaveat appends DegradedReasonLowQualityStaticEmbedder to resp
// when the active embedder is the static-v2 fallback, so the quality cliff
// rides along with every result set (solov2-d2x).
func (s *Service) withEmbedderCaveat(resp Response) Response {
	if s.embedder.ModelID() == staticEmbedderModelID {
		resp.DegradedReasons = append(resp.DegradedReasons, DegradedReasonLowQualityStaticEmbedder)
	}
	return resp
}

// SemanticCandidates returns the per-retriever-ranked, hydrated candidate
// set for query without applying RRF or the name-match rerank. It is the
// fan-in primitive used by the MCP cross-repo handler (solov2-bcn) — the
// handler runs a SINGLE global RRF across every repo's candidates so a
// rank-1 hit in repo A competes fairly with a rank-1 hit in repo B,
// rather than the two top hits both scoring ~1/61 in their local fusion.
//
// The single-repo Semantic path stays on the existing intra-repo RRF +
// rerank pipeline for byte-stability.
//
// Errors: same contract as Semantic (embedder-unreachable falls back to
// the lexical path when available; every other embedder error wraps).
// When the lexical fallback fires the response's DegradedReasons carries
// DegradedReasonEmbedderOfflineLexicalFallback so the caller can render
// the same caveat it would for Semantic.
func (s *Service) SemanticCandidates(ctx context.Context, repoID, branch, query string, k int, filter domain.Filter) (CandidatesResponse, error) {
	if k <= 0 {
		return CandidatesResponse{}, nil
	}

	start := s.now()
	defer s.observe(start)

	vec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		if errors.Is(err, ports.ErrEmbedderUnreachable) && s.lexical != nil {
			results, lerr := s.lexicalFallback(ctx, repoID, branch, query, k)
			if lerr != nil {
				return CandidatesResponse{}, fmt.Errorf("search: lexical fallback after embedder unreachable: %w", lerr)
			}
			cands := make([]RankedCandidate, len(results))
			for i, r := range results {
				cands[i] = RankedCandidate{Result: r, LexicalRank: i + 1}
			}
			return s.withCaveatOnCandidates(CandidatesResponse{
				Candidates:      cands,
				DegradedReasons: []string{DegradedReasonEmbedderOfflineLexicalFallback},
			}), nil
		}
		return CandidatesResponse{}, fmt.Errorf("search: embed query: %w", err)
	}

	const fusionFanout = 3
	const fanoutFloor = 30
	fanK := max(k*fusionFanout, fanoutFloor)
	vecHits, err := s.vectors.Search(ctx, repoID, branch, vec, fanK, filter)
	if err != nil {
		return CandidatesResponse{}, fmt.Errorf("search: vector search: %w", err)
	}
	var lexHits []ports.LexicalHit
	if s.lexical != nil {
		if lh, lerr := s.lexical.Search(ctx, repoID, branch, query, fanK); lerr == nil {
			lexHits = lh
		}
	}
	if len(vecHits) == 0 && len(lexHits) == 0 {
		return s.withCaveatOnCandidates(CandidatesResponse{}), nil
	}

	// Union of node_ids touched by either retriever.
	vecRank := make(map[string]int, len(vecHits))
	vecScore := make(map[string]float32, len(vecHits))
	for i, h := range vecHits {
		vecRank[h.NodeID] = i + 1
		vecScore[h.NodeID] = h.Score
	}
	lexRank := make(map[string]int, len(lexHits))
	for i, h := range lexHits {
		lexRank[h.NodeID] = i + 1
	}
	ids := make([]string, 0, len(vecRank)+len(lexRank))
	seen := make(map[string]struct{}, len(vecRank)+len(lexRank))
	for _, h := range vecHits {
		if _, ok := seen[h.NodeID]; ok {
			continue
		}
		seen[h.NodeID] = struct{}{}
		ids = append(ids, h.NodeID)
	}
	for _, h := range lexHits {
		if _, ok := seen[h.NodeID]; ok {
			continue
		}
		seen[h.NodeID] = struct{}{}
		ids = append(ids, h.NodeID)
	}

	metas, err := s.nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return CandidatesResponse{}, fmt.Errorf("search: node lookup: %w", err)
	}
	byID := make(map[string]ports.NodeMeta, len(metas))
	for _, m := range metas {
		byID[m.NodeID] = m
	}

	out := make([]RankedCandidate, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			continue
		}
		out = append(out, RankedCandidate{
			Result: Result{
				NodeID:     id,
				SymbolPath: m.SymbolPath,
				FilePath:   m.FilePath,
				Kind:       m.Kind,
				LineStart:  m.LineStart,
				LineEnd:    m.LineEnd,
				Snippet:    m.Snippet,
			},
			VectorRank:  vecRank[id],
			LexicalRank: lexRank[id],
			VectorScore: vecScore[id],
		})
	}
	return s.withCaveatOnCandidates(CandidatesResponse{Candidates: out}), nil
}

// withCaveatOnCandidates is the CandidatesResponse twin of
// withEmbedderCaveat — appends the low-quality static-embedder degraded
// reason when applicable so cross-repo callers get the same signal.
func (s *Service) withCaveatOnCandidates(resp CandidatesResponse) CandidatesResponse {
	stub := s.withEmbedderCaveat(Response{DegradedReasons: resp.DegradedReasons})
	resp.DegradedReasons = stub.DegradedReasons
	return resp
}

// observe records a single sample on VectorQueryDuration{kind=semantic_search}.
// When metrics is nil or the histogram is unset the call is a no-op.
func (s *Service) observe(start time.Time) {
	if s.metrics == nil || s.metrics.VectorQueryDuration == nil {
		return
	}
	s.metrics.VectorQueryDuration.WithLabelValues("semantic_search").Observe(s.now().Sub(start).Seconds())
}
