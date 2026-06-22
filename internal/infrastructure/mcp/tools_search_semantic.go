// SPDX-License-Identifier: AGPL-3.0-only

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
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
)

type searchSemanticParams struct {
	Query  string `json:"query"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// K is the result count.
	K     int `json:"k,omitempty"`
	Limit int `json:"limit,omitempty"`
}

func makeSearchSemanticHandler(svc *search.Service, rec *savings.Recorder, repos application.RepoLister, pending PendingEmbedsCounter, ftsPending PendingFTSCounter, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSemanticParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("query", p.Query); rpcErr != nil {
			return nil, rpcErr
		}
		// fanout across registered repos when repo_id is omitted
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
		recordSavings(ctx, rec, repos, p.Query, results, repoByNode, targets[0].RepoID)
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
		// Lexical half of the fusion is partial until the async FTS lane drains
		// (mostly right after a cold scan); flag it so callers don't read a thin
		// keyword-side ranking as authoritative.
		if ftsPending != nil {
			if n, perr := ftsPending.CountPendingFTS(ctx); perr == nil && n > 0 {
				reasons = append(reasons, DegradedReasonFTSPending)
			}
		}
		dtos := searchResultsToDTO(results)
		if fanout {
			for i := range dtos {
				dtos[i].RepoID = repoByNode[dtos[i].NodeID]
			}
		}
		var indexing []string
		// Empty results during active scanning return an indexing degraded reason.
		if len(dtos) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		queriedRepos := make([]string, 0, len(targets))
		for _, tgt := range targets {
			queriedRepos = append(queriedRepos, tgt.RepoID)
		}
		reconciling := reconcilingForRepos(reconcile, queriedRepos)
		if len(reconciling) > 0 {
			reasons = append(reasons, protocol.DegradedReasonWakeReconciling)
		}
		return SearchResponse{Results: dtos, DegradedReasons: reasons, IndexingRepos: indexing, WakeReconcilingRepos: reconciling}, nil
	}
}

// runSemanticFanout queries targets and fuses results using cosine similarity or rank reciprocal fusion.
//
//nolint:funlen,cyclop,revive
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
		resp, err := svc.Semantic(ctx, t.RepoID, t.Branch, query, k, domain.VectorFilter{})
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
			resp, err := svc.SemanticCandidates(ctx, tgt.RepoID, tgt.Branch, query, k, domain.VectorFilter{})
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

	// Cross-repo fusion: when the vector arm returned
	// scores for any candidate - the common case, one daemon = one
	// embedder spanning every repo - fuse by raw cosine similarity
	// rather than RRF. RRF is rank-only, so every repo's vector top-1
	// ties at 1/(60+1) and the cross-repo top-K becomes a coin flip
	// across repos. Cosine scores ARE comparable across repos when the
	// embedder is shared, which makes the fusion actually pick the
	// best match.
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
			// contribution so they survive in the pool - they're
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

// recordSavings records semantic search savings telemetry per repository.
func recordSavings(ctx context.Context, rec *savings.Recorder, repos application.RepoLister, query string, results []search.Result, repoByNode map[string]string, defaultRepoID string) {
	if rec == nil {
		return
	}
	byRepo := map[string][]savings.ResultFile{}
	order := make([]string, 0, 1)
	for _, r := range results {
		repoID := defaultRepoID
		if id, ok := repoByNode[r.NodeID]; ok {
			repoID = id
		}
		if _, seen := byRepo[repoID]; !seen {
			order = append(order, repoID)
		}
		byRepo[repoID] = append(byRepo[repoID], savings.ResultFile{FilePath: r.FilePath, SnippetLen: len(r.Snippet)})
	}
	// result FilePaths are repo-relative, so EntryFor must
	// rejoin each repo's root to stat the file on disk. Resolve roots once.
	rootByRepo := rootPathsByRepoID(ctx, repos)
	now := time.Now()
	for _, repoID := range order {
		_ = rec.Record(savings.EntryFor(repoID, rootByRepo[repoID], query, byRepo[repoID], now))
	}
}

// rootPathsByRepoID resolves and maps repository IDs to their filesystem roots.
func rootPathsByRepoID(ctx context.Context, repos application.RepoLister) map[string]string {
	out := map[string]string{}
	if repos == nil {
		return out
	}
	recs, err := repos.ListRepos(ctx)
	if err != nil {
		return out
	}
	for _, rc := range recs {
		out[rc.RepoID] = rc.RootPath
	}
	return out
}
