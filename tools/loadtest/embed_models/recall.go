//go:build eval

// Recall metric computation for the embed-models bench (solov2-0k5h.3).
// Given a set of ground-truth pairs and an embedded corpus, embed each
// query, find top-K hits, compute recall@1/@5/@10 + MRR.
//
// Naming convention: a hit "matches" a pair when the hit's document
// name equals the pair's Expected symbol. Multiple docs in the corpus
// can share a name (e.g. several New() functions across files); recall
// counts any same-named doc in the top K as a match.

package embed_models

import (
	"context"
	"math"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
)

// RecallScores holds the per-source metric output.
type RecallScores struct {
	N     int     `json:"n"`      // number of pairs evaluated
	At1   float64 `json:"at_1"`   // recall@1
	At5   float64 `json:"at_5"`   // recall@5
	At10  float64 `json:"at_10"`  // recall@10
	MRR   float64 `json:"mrr"`    // mean reciprocal rank (0 when not in top 100)
	Miss  int     `json:"miss"`   // pairs where expected was not in top 100
	Total int     `json:"total"`  // same as N; kept for explicit table output
}

// ComputeRecall embeds every pair.Query with provider, ranks against
// docs, and returns aggregate recall@k + MRR. Pairs whose expected
// symbol does not appear anywhere in docs are still counted (they
// contribute 0 to every metric — the bench measures how well the model
// surfaces a target that's known to be in the corpus, so a missing
// target is a real failure of the corpus or pair set).
func ComputeRecall(provider *model2vec.Provider, pairs []Pair, docs []doc) RecallScores {
	if len(pairs) == 0 || len(docs) == 0 {
		return RecallScores{}
	}
	const probe = 100 // search depth for MRR; cap so a single bad query can't dominate
	var s RecallScores
	s.N = len(pairs)
	s.Total = len(pairs)
	for _, p := range pairs {
		qvec, err := provider.Embed(context.Background(), p.Query)
		if err != nil {
			s.Miss++
			continue
		}
		// Brute-force top-probe by dot product (vectors are L2-normalised).
		bestRank := -1
		topK := topKByDot(qvec, docs, probe, p.Expected)
		if topK >= 0 {
			bestRank = topK
		}
		if bestRank < 0 {
			s.Miss++
			continue
		}
		if bestRank == 0 {
			s.At1++
		}
		if bestRank < 5 {
			s.At5++
		}
		if bestRank < 10 {
			s.At10++
		}
		s.MRR += 1.0 / float64(bestRank+1)
	}
	denom := float64(s.N)
	s.At1 /= denom
	s.At5 /= denom
	s.At10 /= denom
	s.MRR /= denom
	return s
}

// topKByDot returns the 0-based rank of the first doc whose name matches
// expected within the top probe results, or -1 if no match in the top
// probe. We compute the full score list and partial-sort to k=probe so
// we don't allocate a heap structure for small probe sizes — the
// corpus sizes here (≤few thousand docs) make this trivially fast.
func topKByDot(q []float32, docs []doc, probe int, expected string) int {
	type sd struct {
		idx   int
		score float32
	}
	scored := make([]sd, len(docs))
	for i, d := range docs {
		scored[i] = sd{idx: i, score: dot(q, d.vec)}
	}
	// Selection-sort up to probe; cheaper than a full sort for small probe.
	if probe > len(scored) {
		probe = len(scored)
	}
	for i := 0; i < probe; i++ {
		maxIdx := i
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[maxIdx].score {
				maxIdx = j
			}
		}
		scored[i], scored[maxIdx] = scored[maxIdx], scored[i]
		// Early exit if the i-th best is already the expected.
		if docs[scored[i].idx].name == expected {
			return i
		}
		// Guard against NaN/Inf scores polluting the rank.
		if math.IsNaN(float64(scored[i].score)) {
			return -1
		}
	}
	return -1
}
