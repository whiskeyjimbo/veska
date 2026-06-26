// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// CodeFailedPrecondition indicates that a required upstream invariant is not met for a tool execution.
const CodeFailedPrecondition = -32003

// SearchResponse is the envelope returned by the semantic and similar search tools.
type SearchResponse struct {
	Results         []searchHitDTO `json:"results"`
	DegradedReasons []string       `json:"degraded_reasons"`
	// IndexingRepos lists repositories undergoing indexing when the result is degraded.
	IndexingRepos []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos lists repositories undergoing wake reconciliation at query time.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// PendingEmbedsCounter checks the count of pending node embeddings.
type PendingEmbedsCounter interface {
	CountPending(ctx context.Context) (int, error)
}

// PendingFTSCounter checks how many files the async FTS lane has yet to
// reindex. Non-zero means lexical results from eng_search_semantic are partial.
type PendingFTSCounter interface {
	CountPendingFTS(ctx context.Context) (int, error)
}

// DegradedReasonEmbeddingsPending indicates that the daemon has un-embedded nodes queued.
const DegradedReasonEmbeddingsPending = "embeddings_pending"

// DegradedReasonFTSPending indicates the async FTS lane has not finished
// rebuilding the lexical index, so lexical/keyword search results are partial.
const DegradedReasonFTSPending = "fts_pending"

// DegradedReasonLowConfidence is emitted when the top semantic hit's absolute
// RRF score is below search.WeakTopAbsolute - the query landed in only one
// retriever (vector OR lexical, not both), the signature of a precise-logic
// miss. It steers the agent to switch tools (eng_find_symbol / grep for a
// known identifier) rather than re-run the same query, which is the spiral the
// A/B bench measured (P3: +50%..+467% from low-yield repeat semantic calls).
const DegradedReasonLowConfidence = "low_confidence"

// SimilarLookup defines the query interface for checking code similarity by content hash.
type SimilarLookup interface {
	ContentHashForNode(ctx context.Context, repoID, branch, nodeID string) (contentHash string, ready bool, err error)
	LookupExisting(ctx context.Context, contentHash string) (embedding []byte, dim int, found bool, err error)
}

// SearchToolOption configures the search tools registration.
type SearchToolOption func(*searchToolConfig)

type searchToolConfig struct {
	graph      ports.GraphReader
	scans      ScanTrackerReader
	reconcile  ReconcileReader
	ftsPending PendingFTSCounter
}

// WithSearchFTSPending registers the counter that flags partial lexical
// results while the async FTS lane is still draining.
func WithSearchFTSPending(c PendingFTSCounter) SearchToolOption {
	return func(cfg *searchToolConfig) { cfg.ftsPending = c }
}

// WithSearchScanTracker registers the background scan tracker.
func WithSearchScanTracker(t ScanTrackerReader) SearchToolOption {
	return func(c *searchToolConfig) { c.scans = t }
}

// WithSearchReconcileTracker registers the repository reconciliation tracker.
func WithSearchReconcileTracker(t ReconcileReader) SearchToolOption {
	return func(c *searchToolConfig) { c.reconcile = t }
}

// WithSearchGraph registers the graph reader used for symbol resolution in searches.
func WithSearchGraph(g ports.GraphReader) SearchToolOption {
	return func(c *searchToolConfig) { c.graph = g }
}

// defaultSearchK / maxSearchK bound the result count shared by every search
// handler ('limit' is accepted as an alias for k across the surface).
const (
	defaultSearchK = 10
	maxSearchK     = 100
)

// resolveK normalizes search result limit arguments and rejects counts exceeding the maximum.
func resolveK(k, limit int) (int, *RPCError) {
	if k <= 0 {
		k = limit
	}
	if k <= 0 {
		k = defaultSearchK
	}
	if k > maxSearchK {
		return 0, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("k %d exceeds maximum of %d", k, maxSearchK)}
	}
	return k, nil
}

// RegisterSearchTools registers MCP search tools in the registry.
func RegisterSearchTools(
	r *Registry,
	svc *search.Service,
	lookup SimilarLookup,
	vectors ports.VectorStorage,
	nodes ports.NodeLookup,
	rec *savings.Recorder,
	repos application.RepoLister,
	opts ...SearchToolOption,
) {
	var cfg searchToolConfig
	for _, o := range opts {
		o(&cfg)
	}

	var pending PendingEmbedsCounter
	if pc, ok := lookup.(PendingEmbedsCounter); ok {
		pending = pc
	}
	r.MustRegister(ToolSpec{
		Name:            "eng_search_semantic",
		Description:     DescSearchSemantic,
		IncludesStaging: false,
		Tier:            Tier1,
		InputSchema:     searchSemanticInputSchema,
		Handler:         makeSearchSemanticHandler(svc, rec, repos, pending, cfg.ftsPending, cfg.scans, cfg.reconcile),
	})
	// eng_search_similar and eng_find_related are no longer registered here -
	// they merged into eng_find_duplicates (seed=similar / seed=related),
	// registered by RegisterDuplicatesTool. The lookup/vectors/nodes/graph deps
	// flow through to that tool's construction in the daemon wiring.
}
