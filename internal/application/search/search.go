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
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// DegradedReasonEmbedderOfflineLexicalFallback is the canonical token
// emitted on Response.DegradedReasons when Semantic falls back to the
// lexical arm because the embedder was unreachable. The literal matches
// the string defined in SOLO-13 §4 and SOLO-09 §4.5 so MCP/HTTP envelopes
// can forward it unchanged.
const DegradedReasonEmbedderOfflineLexicalFallback = "embedder_offline_lexical_fallback"

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
			return Response{
				Results:         results,
				DegradedReasons: []string{DegradedReasonEmbedderOfflineLexicalFallback},
			}, nil
		}
		return Response{}, fmt.Errorf("search: embed query: %w", err)
	}

	hits, err := s.vectors.Search(ctx, repoID, branch, vec, k, filter)
	if err != nil {
		return Response{}, fmt.Errorf("search: vector search: %w", err)
	}
	if len(hits) == 0 {
		return Response{Results: []Result{}}, nil
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
		})
	}
	return Response{Results: out}, nil
}

// Lexical performs a pure FTS5 lookup (words+trigrams RRF fusion) without
// touching the embedder. Intended for callers that have already decided
// the lexical path is the right one (e.g. an explicit /lexical tool or a
// caller that wants deterministic substring matching). For the
// embedder-fallback case, prefer Semantic — it tags the response with
// degraded_reasons so the agent knows the reasoning mode changed.
func (s *Service) Lexical(ctx context.Context, repoID, branch, query string, k int) ([]Result, error) {
	if k <= 0 || s.lexical == nil {
		return nil, nil
	}
	return s.lexicalFallback(ctx, repoID, branch, query, k)
}

// lexicalFallback runs LexicalSearcher.Search and hydrates the hits via
// NodeLookup. It is the shared body of the Semantic-fallback path and
// the explicit Lexical method.
func (s *Service) lexicalFallback(ctx context.Context, repoID, branch, query string, k int) ([]Result, error) {
	hits, err := s.lexical.Search(ctx, repoID, branch, query, k)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	if len(hits) == 0 {
		return []Result{}, nil
	}

	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.NodeID
	}

	metas, err := s.nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return nil, fmt.Errorf("node lookup: %w", err)
	}
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
			Score:      float32(h.Score),
			SymbolPath: m.SymbolPath,
			FilePath:   m.FilePath,
			Kind:       m.Kind,
			LineStart:  m.LineStart,
			LineEnd:    m.LineEnd,
		})
	}
	return out, nil
}

// observe records a single sample on VectorQueryDuration{kind=semantic_search}.
// When metrics is nil or the histogram is unset the call is a no-op.
func (s *Service) observe(start time.Time) {
	if s.metrics == nil || s.metrics.VectorQueryDuration == nil {
		return
	}
	s.metrics.VectorQueryDuration.WithLabelValues("semantic_search").Observe(s.now().Sub(start).Seconds())
}
