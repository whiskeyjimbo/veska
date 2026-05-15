// Package observability provides Prometheus metrics and OpenTelemetry tracing
// building blocks. All features are opt-in: the HTTP listener only binds when
// the caller explicitly calls StartHTTPListener, and the tracer provider only
// initialises when NewTracerProvider is called with a non-empty endpoint.
package observability

import (
	"context"
	"io"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the six Prometheus metrics defined in SOLO-13 §1.2.
// All metrics are stored as fields so callers can instrument call sites
// directly without a global registry.
type Metrics struct {
	// SealLatency measures end-to-end promotion duration: hook entry → SQL commit
	// → post-promotion queue enqueue. Label: repo_id.
	SealLatency *prometheus.HistogramVec

	// PostCommitHookDuration measures wall-clock from hook entry to hook return.
	// Labels: repo_id, commit_size (typical|refactor).
	PostCommitHookDuration *prometheus.HistogramVec

	// MCPRequestsTotal counts MCP tool calls. Labels: tool, result (ok|error|degraded).
	MCPRequestsTotal *prometheus.CounterVec

	// MCPRequestDuration measures MCP tool handler duration. Labels: tool, result.
	MCPRequestDuration *prometheus.HistogramVec

	// VectorQueryDuration measures sqlite-vec ANN query latency.
	// Label: kind (semantic_search|find_similar_symbols).
	VectorQueryDuration *prometheus.HistogramVec

	// ErrorCount counts errors by kind (promotion|embed|mcp|parse|watcher).
	ErrorCount *prometheus.CounterVec

	// CheckLatency measures the wall-clock duration of each structural check
	// run by the post-promotion check pipeline. Labels: repo_id, check.
	//
	// A sibling histogram (rather than a new label on SealLatency) keeps the
	// end-to-end seal-latency time series clean: SealLatency is a single
	// observation per Promote() call (repo_id), while CheckLatency cardinality
	// fans out per registered check.
	CheckLatency *prometheus.HistogramVec

	// EmbedQueueDepth tracks the number of rows in node_embedding_refs with
	// state='pending'. Sampled once per embedder worker tick. Used to detect
	// backpressure: a rising series means embedding is falling behind
	// promotion.
	EmbedQueueDepth prometheus.Gauge

	// EmbedDedupHits counts the number of pending refs that were resolved
	// against an existing node_embeddings row (by content_hash) WITHOUT
	// calling EmbeddingProvider.Embed. A rising series means content-addressed
	// dedup is saving real Embed work (e.g. two nodes projecting to the same
	// "<kind> <symbol_path>" share one vector).
	EmbedDedupHits prometheus.Counter

	// AutolinkCandidates counts auto-link candidate edges emitted by the
	// similarity service (internal/application/autolink). Label: repo_id.
	// One increment per emitted Candidate, NOT per Linker.Candidates call —
	// a single call may emit zero or many candidates depending on input
	// node count and top-k.
	AutolinkCandidates *prometheus.CounterVec

	// RevalidateClosed counts findings closed by the revalidation sweep
	// (application/revalidate) as 'revalidated_obsolete'. One increment per
	// closed finding (NOT per queue row), so the series tracks real anchor
	// drift, not how often the sweep fires. Unlabeled: revalidation is a
	// system-wide signal — repo/branch fanout is observable via the queue
	// metrics already.
	RevalidateClosed prometheus.Counter

	// RevalidateRefreshed counts findings whose anchor_content_hash was
	// rewritten in place by the revalidation sweep because the rule still
	// fires on the new node content (e.g. dead-code anchor still has no
	// inbound edges; contract-drift anchor still has prev_signature !=
	// signature). One increment per refreshed row. Paired with
	// RevalidateClosed: every stale finding visited resolves to exactly
	// one of (Refreshed | Closed).
	RevalidateRefreshed prometheus.Counter
}

// NewMetrics constructs a Metrics struct and registers all metrics with reg.
// Callers should pass prometheus.NewRegistry() for isolation (tests) or
// prometheus.DefaultRegisterer for the daemon's global registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	sealLatency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "veska_seal_latency_seconds",
			Help: "End-to-end promotion duration: hook entry to SQL commit to post-promotion queue enqueue.",
		},
		[]string{"repo_id"},
	)

	postCommitHookDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "veska_post_commit_hook_duration_seconds",
			Help: "Wall-clock from hook entry to hook return — the user-visible commit latency budget.",
		},
		[]string{"repo_id", "commit_size"},
	)

	mcpRequestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "veska_mcp_requests_total",
			Help: "MCP tool call count. result is ok, error, or degraded.",
		},
		[]string{"tool", "result"},
	)

	mcpRequestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "veska_mcp_request_duration_seconds",
			Help: "MCP tool handler duration.",
		},
		[]string{"tool", "result"},
	)

	vectorQueryDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "veska_vector_query_duration_seconds",
			Help: "sqlite-vec ANN query latency. Decides whether vec0 is still on-budget.",
		},
		[]string{"kind"},
	)

	errorCount := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "veska_error_count",
			Help: "Errors by kind (promotion, embed, mcp, parse, watcher).",
		},
		[]string{"kind"},
	)

	checkLatency := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "veska_check_latency_seconds",
			Help: "Per-structural-check wall-clock duration run by the post-promotion pipeline.",
		},
		[]string{"repo_id", "check"},
	)

	embedQueueDepth := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "veska_embed_queue_depth",
			Help: "Number of node_embedding_refs rows in state='pending'. Sampled per embedder worker tick.",
		},
	)

	embedDedupHits := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "veska_embed_dedup_hits_total",
			Help: "Pending refs resolved by content_hash without calling EmbeddingProvider.Embed.",
		},
	)

	autolinkCandidates := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "veska_autolink_candidates_total",
			Help: "Auto-link candidate edges emitted by the similarity service. Incremented per emitted candidate.",
		},
		[]string{"repo_id"},
	)

	revalidateClosed := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "veska_revalidate_closed_total",
			Help: "Findings closed by the revalidation sweep as 'revalidated_obsolete'. Incremented per closed finding.",
		},
	)

	revalidateRefreshed := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "veska_revalidate_refreshed_total",
			Help: "Findings whose anchor_content_hash was rewritten in place because the rule still fires on the new node content. Incremented per refreshed row.",
		},
	)

	reg.MustRegister(
		sealLatency,
		postCommitHookDuration,
		mcpRequestsTotal,
		mcpRequestDuration,
		vectorQueryDuration,
		errorCount,
		checkLatency,
		embedQueueDepth,
		embedDedupHits,
		autolinkCandidates,
		revalidateClosed,
		revalidateRefreshed,
	)

	return &Metrics{
		SealLatency:            sealLatency,
		PostCommitHookDuration: postCommitHookDuration,
		MCPRequestsTotal:       mcpRequestsTotal,
		MCPRequestDuration:     mcpRequestDuration,
		VectorQueryDuration:    vectorQueryDuration,
		ErrorCount:             errorCount,
		CheckLatency:           checkLatency,
		EmbedQueueDepth:        embedQueueDepth,
		EmbedDedupHits:         embedDedupHits,
		AutolinkCandidates:     autolinkCandidates,
		RevalidateClosed:       revalidateClosed,
		RevalidateRefreshed:    revalidateRefreshed,
	}
}

// MetricsHandler returns an http.Handler that serves Prometheus metrics for reg.
// The caller may use this with httptest.NewServer in tests or wire it into a
// net/http.ServeMux for production use.
func MetricsHandler(reg prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// httpCloser wraps an http.Server so io.Closer shuts it down gracefully.
type httpCloser struct {
	srv *http.Server
}

func (c *httpCloser) Close() error {
	return c.srv.Shutdown(context.Background())
}

// StartHTTPListener binds an HTTP listener on addr and serves /metrics from reg.
// The returned io.Closer shuts the listener down gracefully.
// addr may be "127.0.0.1:0" to let the OS pick a free port.
// The caller is responsible for checking config before calling this function —
// it binds immediately.
func StartHTTPListener(addr string, reg interface {
	prometheus.Registerer
	prometheus.Gatherer
}) (io.Closer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", MetricsHandler(reg))

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	return &httpCloser{srv: srv}, nil
}
