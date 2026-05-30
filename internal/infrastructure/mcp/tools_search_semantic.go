package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// This file holds the eng_search_semantic handler and its cross-repo fanout
// fusion. The tool registration, shared response types, and options live in
// tools_search.go; the similar / find_related handlers live in
// tools_search_similar.go.

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
		k, rpcErr := resolveK(p.K, p.Limit)
		if rpcErr != nil {
			return nil, rpcErr
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
//
// This is a verbatim relocation of the pre-existing fanout/fusion logic
// (solov2-bcn/uuuk), unchanged by the file split. The per-function size gates
// are diff-scoped (--new-from-merge-base) and only flag it because the move
// makes git see it as new code; restructuring this fusion path to satisfy them
// would add behaviour risk for no benefit.
//
//nolint:funlen,cyclop,revive // see note above: verbatim relocation, diff-scoped gate
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
	for key := range scores {
		keys = append(keys, key)
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
