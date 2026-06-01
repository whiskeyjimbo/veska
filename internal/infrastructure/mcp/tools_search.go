package mcp

import (
	"context"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// This file registers the search tool family and holds the types shared across
// its handlers. The handler implementations are split by concern:
// eng_search_semantic lives in tools_search_semantic.go; eng_search_similar and
// eng_find_related live in tools_search_similar.go.

// CodeFailedPrecondition is returned when a tool cannot proceed because a
// required upstream invariant is not met (e.g. similar-search against a node
// that has not yet been embedded).
const CodeFailedPrecondition = -32003

// SearchResponse is the envelope returned by eng_search_semantic and
// eng_search_similar. DegradedReasons forwards lexical-fallback markers
// from search.Service unchanged so callers can branch on the mode that
// actually serviced the query.
// SearchResponse fields use non-omitempty tags so the wire shape is
// stable across calls — empty collections serialize as [] per the
// README's "Conventions across the tool surface" contract .
type SearchResponse struct {
	Results         []searchHitDTO `json:"results"`
	DegradedReasons []string       `json:"degraded_reasons"`
	// IndexingRepos populates alongside DegradedReason "indexing_in_progress"
	// when a cold scan is in flight at query time and the result is empty
	// (solov2-izh6.30). Omitted from JSON when empty.
	IndexingRepos []string `json:"indexing_repos,omitempty"`
}

// PendingEmbedsCounter exposes the global pending-embeds depth so the
// semantic handler can tag responses with 'embeddings_pending' while the
// index is still warming. nil is a no-op .
type PendingEmbedsCounter interface {
	CountPending(ctx context.Context) (int, error)
}

// DegradedReasonEmbeddingsPending is the canonical token emitted on
// eng_search_semantic responses when the daemon still has un-embedded
// nodes queued. A junior running a search against a freshly-registered
// repo and getting [] otherwise has no signal that the index is warming
// rather than the query being wrong.
const DegradedReasonEmbeddingsPending = "embeddings_pending"

// SimilarLookup is the narrow port the eng_search_similar handler needs from
// EmbeddingRefRepo: given a node, return its content_hash if ready, and given
// a content_hash, return the stored embedding bytes + dimension. This
// interface is satisfied by *sqlite.EmbeddingRefsRepo without modification.
type SimilarLookup interface {
	ContentHashForNode(ctx context.Context, repoID, branch, nodeID string) (contentHash string, ready bool, err error)
	LookupExisting(ctx context.Context, contentHash string) (embedding []byte, dim int, found bool, err error)
}

// SearchToolOption configures RegisterSearchTools. The only knob today is
// the GraphStorage used by eng_search_similar to resolve a `symbol` param
// to a node_id ; composition roots that don't wire it can
// still call the tool with node_id directly.
type SearchToolOption func(*searchToolConfig)

type searchToolConfig struct {
	graph ports.GraphReader
	scans ScanTrackerReader
}

// WithSearchScanTracker supplies the daemon's cold-scan tracker so empty
// search responses can carry an indexing_in_progress hint when a scan is
// in flight (solov2-izh6.30). Nil disables the hint.
func WithSearchScanTracker(t ScanTrackerReader) SearchToolOption {
	return func(c *searchToolConfig) { c.scans = t }
}

// WithSearchGraph supplies the GraphStorage used by eng_search_similar's
// symbol-to-node_id resolution. Without it, `symbol` is rejected and only
// node_id is accepted — preserving existing behaviour for callers that
// don't pass the option.
func WithSearchGraph(g ports.GraphReader) SearchToolOption {
	return func(c *searchToolConfig) { c.graph = g }
}

// defaultSearchK / maxSearchK bound the result count shared by every search
// handler ('limit' is accepted as an alias for k across the surface).
const (
	defaultSearchK = 10
	maxSearchK     = 100
)

// resolveK normalises the k / limit aliases shared by every search handler:
// k wins, 'limit' is the fallback alias , zero/negative means the
// default, and anything above maxSearchK is rejected. Centralising it keeps the
// three handlers byte-identical on this contract instead of triplicating it.
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

// RegisterSearchTools registers eng_search_semantic and eng_search_similar.
// svc is required and orchestrates the semantic + lexical-fallback path.
// lookup + vectors + nodes drive the similar-by-node-id path. rec is
// optional: a nil recorder disables savings telemetry .
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
	// solov2-hjw9: opportunistically extract a PendingEmbedsCounter from the
	// SimilarLookup. *sqlite.EmbeddingRefsRepo satisfies both interfaces; test
	// stubs that don't can ignore the signal (handler treats nil as "no info").
	var pending PendingEmbedsCounter
	if pc, ok := lookup.(PendingEmbedsCounter); ok {
		pending = pc
	}
	r.MustRegister(ToolSpec{
		Name:            "eng_search_semantic",
		Description:     DescSearchSemantic,
		IncludesStaging: false,
		InputSchema:     searchSemanticInputSchema,
		Handler:         makeSearchSemanticHandler(svc, rec, repos, pending, cfg.scans),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_search_similar",
		Description:     "Vector-nearest-neighbour search seeded by an existing symbol's embedding — 'what else looks like this?'. Use after eng_find_symbol or eng_search_semantic when you want to find variants, near-duplicates, or candidate refactor targets. Accepts node_id (exact) or symbol (resolved via FindNodes). Excludes the seed itself from results.",
		IncludesStaging: false,
		InputSchema:     searchSimilarInputSchema,
		Handler:         makeSearchSimilarHandler(lookup, vectors, nodes, repos, cfg.graph),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_find_related",
		Description:     "Find symbols semantically similar to the code at a given (file_path, line). Use as a moat-pivot from a search hit, an error trace, or an open editor cursor: 'what else in the graph looks like this?'. Resolves the smallest enclosing symbol or chunk for the given line, then runs the same vector-neighbourhood search as eng_search_similar — no separate find_symbol round-trip needed.",
		IncludesStaging: false,
		InputSchema:     findRelatedInputSchema,
		Handler:         makeFindRelatedHandler(lookup, vectors, nodes, repos),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
}
