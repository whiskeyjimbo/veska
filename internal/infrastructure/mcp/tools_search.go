package mcp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/savings"
)

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
// README's "Conventions across the tool surface" contract (solov2-2bdj).
type SearchResponse struct {
	Results         []searchHitDTO `json:"results"`
	DegradedReasons []string       `json:"degraded_reasons"`
}

// PendingEmbedsCounter exposes the global pending-embeds depth so the
// semantic handler can tag responses with 'embeddings_pending' while the
// index is still warming. nil is a no-op (solov2-hjw9).
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

// RegisterSearchTools registers eng_search_semantic and eng_search_similar.
// svc is required and orchestrates the semantic + lexical-fallback path.
// lookup + vectors + nodes drive the similar-by-node-id path. rec is
// optional: a nil recorder disables savings telemetry (solov2-3bu).
func RegisterSearchTools(
	r *Registry,
	svc *search.Service,
	lookup SimilarLookup,
	vectors ports.VectorStorage,
	nodes ports.NodeLookup,
	rec *savings.Recorder,
	repos application.RepoLister,
) {
	// solov2-hjw9: opportunistically extract a PendingEmbedsCounter from the
	// SimilarLookup. *sqlite.EmbeddingRefsRepo satisfies both interfaces; test
	// stubs that don't can ignore the signal (handler treats nil as "no info").
	var pending PendingEmbedsCounter
	if pc, ok := lookup.(PendingEmbedsCounter); ok {
		pending = pc
	}
	r.MustRegister(ToolSpec{
		Name:            "eng_search_semantic",
		Description:     "Semantic search over embedded symbols with lexical fallback when the embedder is offline.",
		IncludesStaging: false,
		Handler:         makeSearchSemanticHandler(svc, rec, repos, pending),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_search_similar",
		Description:     "Find symbols similar to a given node by vector neighbourhood over its stored embedding.",
		IncludesStaging: false,
		Handler:         makeSearchSimilarHandler(lookup, vectors, nodes, repos),
	})
}

const defaultSearchK = 10
const maxSearchK = 100

type searchSemanticParams struct {
	Query  string `json:"query"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// K is the result count. 'limit' is accepted as an alias because
	// every other MCP tool we expose uses 'limit' and callers naturally
	// reach for it first (solov2-8rm). When both are set, K wins.
	K     int `json:"k,omitempty"`
	Limit int `json:"limit,omitempty"`
}

func makeSearchSemanticHandler(svc *search.Service, rec *savings.Recorder, repos application.RepoLister, pending PendingEmbedsCounter) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSemanticParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("query", p.Query); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ktz0: fall back to shim-injected cwd when repo_id omitted.
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}
		k := p.K
		if k <= 0 {
			k = p.Limit
		}
		if k <= 0 {
			k = defaultSearchK
		}
		if k > maxSearchK {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("k %d exceeds maximum of %d", k, maxSearchK)}
		}

		resp, err := svc.Semantic(ctx, p.RepoID, p.Branch, p.Query, k, domain.Filter{})
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("semantic search: %v", err)}
		}
		results := resp.Results
		if results == nil {
			results = []search.Result{}
		}
		recordSavings(rec, p.Query, results)
		reasons := resp.DegradedReasons
		if pending != nil {
			if n, perr := pending.CountPending(ctx); perr == nil && n > 0 {
				reasons = append(reasons, DegradedReasonEmbeddingsPending)
			}
		}
		if reasons == nil {
			reasons = []string{}
		}
		return SearchResponse{Results: searchResultsToDTO(results), DegradedReasons: reasons}, nil
	}
}

// recordSavings is the savings-telemetry side-effect for a successful
// semantic search. It is intentionally fire-and-forget: a write error
// is silently dropped so the search hot path never fails for telemetry
// reasons, and a nil recorder is a no-op (handled inside Record).
func recordSavings(rec *savings.Recorder, query string, results []search.Result) {
	if rec == nil {
		return
	}
	rf := make([]savings.ResultFile, len(results))
	for i, r := range results {
		rf[i] = savings.ResultFile{FilePath: r.FilePath, SnippetLen: len(r.Snippet)}
	}
	_ = rec.Record(savings.EntryFor(query, rf, time.Now()))
}

type searchSimilarParams struct {
	NodeID string `json:"node_id"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// K is the neighbour count. 'limit' accepted as an alias — see
	// searchSemanticParams for rationale (solov2-8rm).
	K     int `json:"k,omitempty"`
	Limit int `json:"limit,omitempty"`
}

func makeSearchSimilarHandler(lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSimilarParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("node_id", p.NodeID); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ktz0: fall back to shim-injected cwd when repo_id omitted.
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}
		k := p.K
		if k <= 0 {
			k = p.Limit
		}
		if k <= 0 {
			k = defaultSearchK
		}
		if k > maxSearchK {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("k %d exceeds maximum of %d", k, maxSearchK)}
		}

		hash, ready, err := lookup.ContentHashForNode(ctx, p.RepoID, p.Branch, p.NodeID)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: content hash lookup: %v", err)}
		}
		if !ready || hash == "" {
			return nil, &RPCError{
				Code:    CodeFailedPrecondition,
				Message: "node has no embedding",
				Data:    map[string]any{"reason": "node_not_embedded", "node_id": p.NodeID},
			}
		}
		blob, dim, found, err := lookup.LookupExisting(ctx, hash)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: embedding lookup: %v", err)}
		}
		if !found || dim == 0 {
			return nil, &RPCError{
				Code:    CodeFailedPrecondition,
				Message: "node has no embedding",
				Data:    map[string]any{"reason": "node_not_embedded", "node_id": p.NodeID},
			}
		}
		vec := decodeFloat32LE(blob, dim)

		// Over-request by one so we can filter the seed node out of results
		// and still return k neighbours (the seed is its own nearest match).
		hits, err := vectors.Search(ctx, p.RepoID, p.Branch, vec, k+1, domain.Filter{})
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: vector search: %v", err)}
		}
		filtered := make([]domain.Hit, 0, len(hits))
		for _, h := range hits {
			if h.NodeID == p.NodeID {
				continue
			}
			filtered = append(filtered, h)
			if len(filtered) >= k {
				break
			}
		}

		ids := make([]string, len(filtered))
		for i, h := range filtered {
			ids[i] = h.NodeID
		}
		metas, err := nodes.LookupNodes(ctx, p.RepoID, p.Branch, ids)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: node lookup: %v", err)}
		}
		byID := make(map[string]ports.NodeMeta, len(metas))
		for _, m := range metas {
			byID[m.NodeID] = m
		}
		out := make([]search.Result, 0, len(filtered))
		for _, h := range filtered {
			m, ok := byID[h.NodeID]
			if !ok {
				continue
			}
			out = append(out, search.Result{
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
		return SearchResponse{Results: searchResultsToDTO(out), DegradedReasons: []string{}}, nil
	}
}

// decodeFloat32LE reverses the little-endian float32 packing used by
// node_embeddings.embedding. Mirrors the helper in application/embedder and
// application/autolink — duplicated to avoid a cross-package import from the
// MCP layer into application internals.
func decodeFloat32LE(blob []byte, dim int) []float32 {
	have := len(blob) / 4
	if have < dim {
		dim = have
	}
	out := make([]float32, dim)
	for i := range dim {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4 : i*4+4]))
	}
	return out
}
