package mcp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
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
	// IndexingRepos populates alongside DegradedReason "indexing_in_progress"
	// when a cold scan is in flight at query time and the result is empty
	// (solov2-izh6.30). Omitted from JSON when empty.
	IndexingRepos []string `json:"indexing_repos,omitempty"`
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

// SearchToolOption configures RegisterSearchTools. The only knob today is
// the GraphStorage used by eng_search_similar to resolve a `symbol` param
// to a node_id (solov2-3ocy); composition roots that don't wire it can
// still call the tool with node_id directly.
type SearchToolOption func(*searchToolConfig)

type searchToolConfig struct {
	graph ports.GraphStorage
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
func WithSearchGraph(g ports.GraphStorage) SearchToolOption {
	return func(c *searchToolConfig) { c.graph = g }
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

func makeSearchSemanticHandler(svc *search.Service, rec *savings.Recorder, repos application.RepoLister, pending PendingEmbedsCounter, scans ScanTrackerReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSemanticParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("query", p.Query); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-g8fh: fanout across registered repos when repo_id is omitted
		// and cwd doesn't match one. Single-repo callers are unchanged.
		targets, fanout, rpcErr := resolveRepoFanoutFromParams(ctx, repos, raw, p.RepoID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
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

		results, repoByNode, reasonsSet, rpcErr := runSemanticFanout(ctx, svc, targets, p.Query, k, fanout)
		if rpcErr != nil {
			return nil, rpcErr
		}
		recordSavings(rec, p.Query, results)
		reasons := make([]string, 0, len(reasonsSet))
		for r := range reasonsSet {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		if pending != nil {
			if n, perr := pending.CountPending(ctx); perr == nil && n > 0 {
				reasons = append(reasons, DegradedReasonEmbeddingsPending)
			}
		}
		dtos := searchResultsToDTO(results)
		if fanout {
			for i := range dtos {
				dtos[i].RepoID = repoByNode[dtos[i].NodeID]
			}
		}
		var indexing []string
		// solov2-izh6.30: empty search result during an active cold scan
		// is the indexing window. Surface it so a junior who just ran
		// 'veska search' on a freshly-added repo knows to retry.
		if len(dtos) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		return SearchResponse{Results: dtos, DegradedReasons: reasons, IndexingRepos: indexing}, nil
	}
}

// runSemanticFanout dispatches a semantic-search query across one or
// more (repo_id, branch) targets and returns the top-K results.
//
// Single-repo (fanout=false): the existing svc.Semantic pipeline runs —
// intra-repo RRF + post-fusion rerank — and the response is returned
// verbatim. Byte-stable with the pre-bcn behaviour.
//
// Multi-repo (fanout=true, solov2-bcn): every repo is queried in
// parallel via svc.SemanticCandidates which returns un-fused, hydrated
// candidates with per-retriever ranks AND raw vector scores. When any
// candidate carries a vector score — the common case, one daemon =
// one embedder spanning every repo — the pool is fused by COSINE
// SIMILARITY (solov2-uuuk) so a stronger match in repo A beats a
// weaker one in repo B even though both ranked 1 locally. Lexical
// confirms a candidate via a small multiplier; lexical-only
// candidates survive via a small RRF baseline. When no vector score
// is available, falls back to the original global RRF.
//
// Returns (results, repoByNode, reasons). repoByNode keys hits to the
// repo they came from so the handler can populate per-hit repo_id.
func runSemanticFanout(
	ctx context.Context,
	svc *search.Service,
	targets []repoBranch,
	query string,
	k int,
	fanout bool,
) ([]search.Result, map[string]string, map[string]struct{}, *RPCError) {
	reasonsSet := map[string]struct{}{}

	if !fanout {
		// Single target: keep the existing within-repo pipeline.
		t := targets[0]
		resp, err := svc.Semantic(ctx, t.RepoID, t.Branch, query, k, domain.Filter{})
		if err != nil {
			return nil, nil, nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("semantic search: %v", err)}
		}
		for _, r := range resp.DegradedReasons {
			reasonsSet[r] = struct{}{}
		}
		return resp.Results, nil, reasonsSet, nil
	}

	// Parallel fanout: collect candidates from every repo concurrently
	// then global-RRF below.
	type repoResult struct {
		repoID string
		resp   search.CandidatesResponse
		err    error
	}
	results := make([]repoResult, len(targets))
	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		go func(i int, tgt repoBranch) {
			defer wg.Done()
			resp, err := svc.SemanticCandidates(ctx, tgt.RepoID, tgt.Branch, query, k, domain.Filter{})
			results[i] = repoResult{repoID: tgt.RepoID, resp: resp, err: err}
		}(i, tgt)
	}
	wg.Wait()

	type pooledCand struct {
		repoID string
		cand   search.RankedCandidate
	}
	var pool []pooledCand
	for _, rr := range results {
		if rr.err != nil {
			// Per-repo failure mirrors the cross-repo CLI policy
			// (cmd/veska/search.go daemonSearchAllRepos): degrade,
			// don't abort. Surface as a degraded reason so the
			// caller can tell partial-fanout from clean-fanout.
			reasonsSet[fmt.Sprintf("repo_%s_unavailable", ShortRepoID(rr.repoID))] = struct{}{}
			continue
		}
		for _, c := range rr.resp.Candidates {
			pool = append(pool, pooledCand{repoID: rr.repoID, cand: c})
		}
		for _, r := range rr.resp.DegradedReasons {
			reasonsSet[r] = struct{}{}
		}
	}
	if len(pool) == 0 {
		return nil, nil, reasonsSet, nil
	}

	// Cross-repo fusion (solov2-uuuk): when the vector arm returned
	// scores for any candidate — the common case, one daemon = one
	// embedder spanning every repo — fuse by raw cosine similarity
	// rather than RRF. RRF is rank-only, so every repo's vector top-1
	// ties at 1/(60+1) and the cross-repo top-K becomes a coin flip
	// across repos. Cosine scores ARE comparable across repos when the
	// embedder is shared, which makes the fusion actually pick the
	// best match.
	//
	// Falls back to the original global RRF when no candidate has a
	// vector score (every repo's vector arm failed, or every hit came
	// from the lexical arm only).
	useCosine := false
	for _, pc := range pool {
		if pc.cand.VectorScore > 0 {
			useCosine = true
			break
		}
	}
	const rrfConstant = 60
	scores := make(map[string]float32, len(pool))
	candByNode := make(map[string]pooledCand, len(pool))
	for _, pc := range pool {
		nodeKey := pc.repoID + ":" + pc.cand.NodeID
		if _, exists := candByNode[nodeKey]; !exists {
			candByNode[nodeKey] = pc
		}
		if useCosine {
			// Cosine fusion: vector score is the primary signal;
			// lexical co-occurrence adds a small bonus so a
			// candidate confirmed by both retrievers beats a
			// vector-only candidate of equal score. Lexical-only
			// candidates (vector miss) get a baseline RRF
			// contribution so they survive in the pool — they're
			// rare but useful when the embedder misses a
			// keyword-heavy query.
			if pc.cand.VectorScore > 0 {
				scores[nodeKey] = pc.cand.VectorScore
				if pc.cand.LexicalRank > 0 {
					scores[nodeKey] *= 1.05
				}
			} else if pc.cand.LexicalRank > 0 {
				scores[nodeKey] = 1.0 / float32(rrfConstant+pc.cand.LexicalRank)
			}
		} else {
			if pc.cand.VectorRank > 0 {
				scores[nodeKey] += 1.0 / float32(rrfConstant+pc.cand.VectorRank)
			}
			if pc.cand.LexicalRank > 0 {
				scores[nodeKey] += 1.0 / float32(rrfConstant+pc.cand.LexicalRank)
			}
		}
	}

	// Stable sort: deterministic order when scores tie (small per-repo
	// candidate sets routinely tie at the top).
	keys := make([]string, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		si, sj := scores[keys[i]], scores[keys[j]]
		if si != sj {
			return si > sj
		}
		return keys[i] < keys[j]
	})
	if len(keys) > k {
		keys = keys[:k]
	}

	out := make([]search.Result, 0, len(keys))
	repoByNode := make(map[string]string, len(keys))
	for _, key := range keys {
		pc := candByNode[key]
		r := pc.cand.Result
		r.Score = scores[key]
		out = append(out, r)
		repoByNode[r.NodeID] = pc.repoID
	}
	return out, repoByNode, reasonsSet, nil
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
	// Symbol is an alias for node_id, resolved via GraphStorage.FindNodes.
	// Parity with eng_find_symbol / eng_get_call_chain / eng_get_blast_radius
	// (solov2-3ocy). Ambiguous matches are rejected so the caller must
	// disambiguate via node_id.
	Symbol string `json:"symbol"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// K is the neighbour count. 'limit' accepted as an alias — see
	// searchSemanticParams for rationale (solov2-8rm).
	K     int `json:"k,omitempty"`
	Limit int `json:"limit,omitempty"`
}

func makeSearchSimilarHandler(lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repos application.RepoLister, graph ports.GraphStorage) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSimilarParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if p.NodeID == "" && p.Symbol == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "missing required params: node_id or symbol"}
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
		// solov2-3ocy: resolve `symbol` to node_id when supplied. Same
		// shape as eng_get_blast_radius — node_id wins when both are set;
		// ambiguity is rejected so callers must disambiguate explicitly.
		if p.NodeID == "" {
			if graph == nil {
				return nil, &RPCError{Code: CodeInternalError, Message: "symbol lookup not wired (graph storage missing); pass node_id"}
			}
			matches, ferr := graph.FindNodes(ctx, p.RepoID, p.Branch, p.Symbol)
			if ferr != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find symbol %q: %v", p.Symbol, ferr)}
			}
			if len(matches) == 0 {
				return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("symbol not found: %s", p.Symbol)}
			}
			if len(matches) > 1 {
				return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("symbol %q is ambiguous (%d matches); pass node_id to disambiguate", p.Symbol, len(matches))}
			}
			p.NodeID = string(matches[0].ID)
		}
		// solov2-xc7t: callers commonly scrape the 12-char short_id from a
		// previous tool's CLI output and feed it straight back. Expand any
		// non-canonical-length id to its full form before the embedding
		// lookup so a short_id surfaces as "no node matches prefix" instead
		// of the misleading "node has no embedding".
		full, rpcErr := expandNodeIDPrefix(ctx, graph, p.RepoID, p.Branch, p.NodeID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.NodeID = full
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

		results, rpcErr2 := findSimilarByNodeID(ctx, lookup, vectors, nodes, p.RepoID, p.Branch, p.NodeID, k)
		if rpcErr2 != nil {
			return nil, rpcErr2
		}
		return SearchResponse{Results: searchResultsToDTO(results), DegradedReasons: []string{}}, nil
	}
}

// findSimilarByNodeID is the shared core of eng_search_similar and
// eng_find_related (solov2-2g4r). Given a seed node_id, it pulls the
// stored embedding, runs a k-NN vector search, filters the seed out,
// and hydrates the hits into search.Result records. The seed-filter
// over-requests by one neighbour so the caller still gets k results.
func findSimilarByNodeID(ctx context.Context, lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repoID, branch, nodeID string, k int) ([]search.Result, *RPCError) {
	hash, ready, err := lookup.ContentHashForNode(ctx, repoID, branch, nodeID)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: content hash lookup: %v", err)}
	}
	if !ready || hash == "" {
		return nil, &RPCError{
			Code:    CodeFailedPrecondition,
			Message: "node has no embedding",
			Data:    map[string]any{"reason": "node_not_embedded", "node_id": nodeID},
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
			Data:    map[string]any{"reason": "node_not_embedded", "node_id": nodeID},
		}
	}
	vec := decodeFloat32LE(blob, dim)

	// Over-request by one so we can filter the seed node out of results
	// and still return k neighbours (the seed is its own nearest match).
	hits, err := vectors.Search(ctx, repoID, branch, vec, k+1, domain.Filter{})
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: vector search: %v", err)}
	}
	filtered := make([]domain.Hit, 0, len(hits))
	for _, h := range hits {
		if h.NodeID == nodeID {
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
	metas, err := nodes.LookupNodes(ctx, repoID, branch, ids)
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
	return out, nil
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

// findRelatedInputSchema declares the (file_path, line) anchor for the
// eng_find_related tool (solov2-2g4r). Line is 1-indexed to match every
// other line-aware contract on the surface.
var findRelatedInputSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Find symbols semantically similar to the code at a (file_path, line). The handler resolves the smallest enclosing node and reuses the eng_search_similar vector-neighbourhood path. Line is 1-indexed.",
  "properties": {
    "file_path": {"type": "string", "description": "Absolute path or repo-relative path to the file."},
    "line":      {"type": "integer", "minimum": 1, "description": "1-indexed source line; the enclosing node's embedding is the seed."},
    "repo_id":   {"type": "string"},
    "branch":    {"type": "string"},
    "k":         {"type": "integer", "minimum": 1, "description": "Neighbour count (default 10). 'limit' is accepted as an alias."},
    "limit":     {"type": "integer", "minimum": 1, "description": "Alias for k."},
    "cwd":       {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  },
  "required": ["file_path", "line"]
}`)

type findRelatedParams struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	RepoID   string `json:"repo_id"`
	Branch   string `json:"branch"`
	K        int    `json:"k,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// makeFindRelatedHandler resolves (file_path, line) into the smallest
// enclosing node and delegates to findSimilarByNodeID. solov2-2g4r.
//
// "Smallest enclosing" handles TS-style nesting (class containing
// method) and intra-Go cases where a chunk and a function both cover
// the same line — picking the tightest span gives the agent the most
// specific embedding to anchor on. Chunks ARE eligible anchors because
// the user might point at a non-symbol region (a top-of-file comment,
// an init block, raw config) and "what else looks like this" is still
// a meaningful question there.
func makeFindRelatedHandler(lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findRelatedParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if p.FilePath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "file_path is required"}
		}
		if p.Line < 1 {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "line must be >= 1 (lines are 1-indexed)"}
		}
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

		nodeID, rpcErr := resolveEnclosingNode(ctx, nodes, p.RepoID, p.Branch, p.FilePath, p.Line)
		if rpcErr != nil {
			return nil, rpcErr
		}

		results, rpcErr := findSimilarByNodeID(ctx, lookup, vectors, nodes, p.RepoID, p.Branch, nodeID, k)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return SearchResponse{Results: searchResultsToDTO(results), DegradedReasons: []string{}}, nil
	}
}

// resolveEnclosingNode picks the smallest line-span node whose range
// covers `line` in `filePath`. Returns CodeNotFound when no node
// matches (the file is unparsed, the line lies in pre-package
// whitespace, or the path doesn't belong to the repo). solov2-2g4r.
func resolveEnclosingNode(ctx context.Context, nodes ports.NodeLookup, repoID, branch, filePath string, line int) (string, *RPCError) {
	ids, err := nodes.NodesInFile(ctx, repoID, branch, filePath)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("nodes in file: %v", err)}
	}
	if len(ids) == 0 {
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("no nodes indexed for file_path=%q in repo=%s; check that the file is part of a registered repo and has been promoted", filePath, repoID)}
	}
	metas, err := nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("hydrate nodes: %v", err)}
	}
	bestID := ""
	bestSpan := math.MaxInt
	for _, m := range metas {
		if m.LineStart <= 0 || m.LineEnd <= 0 {
			continue
		}
		if line < m.LineStart || line > m.LineEnd {
			continue
		}
		span := m.LineEnd - m.LineStart + 1
		if span < bestSpan {
			bestSpan = span
			bestID = m.NodeID
		}
	}
	if bestID == "" {
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("no symbol or chunk covers %s:%d (line lies in whitespace or outside any indexed range)", filePath, line)}
	}
	return bestID, nil
}
