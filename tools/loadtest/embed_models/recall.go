//go:build eval

// Recall metric computation for the embed-models bench.
// Given a set of ground-truth pairs and an embedded corpus, embed each
// query, find top-K hits, compute recall@1/@5/@10 + MRR.
// Naming convention: a hit "matches" a pair when the hit's document
// name equals the pair's Expected symbol. Multiple docs in the corpus
// can share a name (e.g. several New functions across files); recall
// counts any same-named doc in the top K as a match.

package embed_models

import (
	"context"
	"math"
)

// RecallScores holds the per-source metric output.
// Two recall series are reported. RAW recall@k uses N (all pairs) as the
// denominator — penalises models whose target docs weren't embedded at all
// (e.g. an Ollama run capped at max_docs=500 against a 2489-doc corpus
// where the headline target lives outside the first 500). FAIR recall@k
// uses (N - NotInCorpus) — recall over pairs whose target was actually
// in the embedded subset, so cross-type comparisons (model2vec vs ollama,
// where the corpus caps differ by design) aren't biased by the cap.
// Reporters should prefer FairAt10 when comparing models with different
// embed-time corpora sizes; the raw numbers stay useful for tracking
// fixture quality (not_in_corpus shifting between runs == fixture or
// model changes).
type RecallScores struct {
	N           int     `json:"n"`             // number of pairs evaluated (incl. not-in-corpus)
	At1         float64 `json:"at_1"`          // recall@1 (raw, denom=N)
	At5         float64 `json:"at_5"`          // recall@5 (raw, denom=N)
	At10        float64 `json:"at_10"`         // recall@10 (raw, denom=N)
	MRR         float64 `json:"mrr"`           // mean reciprocal rank (raw)
	FairAt1     float64 `json:"fair_at_1"`     // recall@1 over pairs whose target IS in the embedded subset
	FairAt5     float64 `json:"fair_at_5"`     // recall@5 (fair denom = N - NotInCorpus)
	FairAt10    float64 `json:"fair_at_10"`    // recall@10 (fair)
	FairMRR     float64 `json:"fair_mrr"`      // MRR (fair)
	FairN       int     `json:"fair_n"`        // N - NotInCorpus (pairs that COULD have matched)
	Miss        int     `json:"miss"`          // pairs where expected was not in top 100
	Total       int     `json:"total"`         // same as N; kept for explicit table output
	NotInCorpus int     `json:"not_in_corpus"` // pairs whose expected name is absent from the embedded docs entirely
}

// ComputeRecall embeds every pair.Query with provider, ranks against
// docs, and returns aggregate recall@k + MRR. Pairs whose expected
// symbol does not appear anywhere in docs are still counted (they
// contribute 0 to every metric — the bench measures how well the model
// surfaces a target that's known to be in the corpus, so a missing
// target is a real failure of the corpus or pair set).
func ComputeRecall(provider Embedder, pairs []Pair, docs []doc) RecallScores {
	if len(pairs) == 0 || len(docs) == 0 {
		return RecallScores{}
	}
	const probe = 100 // search depth for MRR; cap so a single bad query can't dominate
	var s RecallScores
	s.N = len(pairs)
	s.Total = len(pairs)
	// Precompute the set of names present in the corpus so we can
	// distinguish "fixture references a name that doesn't exist" from
	// "model failed to rank the right name highly" — different
	// remediation paths for the bench curator.
	corpusNames := make(map[string]bool, len(docs))
	for _, d := range docs {
		corpusNames[d.name] = true
	}
	for _, p := range pairs {
		if !corpusNames[p.Expected] {
			s.NotInCorpus++
			s.Miss++
			continue
		}
		qvec, err := provider.Embed(context.Background(), p.Query)
		if err != nil {
			s.Miss++
			continue
		}
		// Brute-force top-probe by dot product (vectors are L2-normalised).
		bestRank := topKByDot(qvec, docs, probe, p.Expected)
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
	// Raw series: denominator = N (includes not-in-corpus).
	denom := float64(s.N)
	// Stash counts of in-top-K hits before normalising so we can also
	// compute the fair series with a different denominator.
	hits1, hits5, hits10, sumRR := s.At1, s.At5, s.At10, s.MRR
	s.At1 = hits1 / denom
	s.At5 = hits5 / denom
	s.At10 = hits10 / denom
	s.MRR = sumRR / denom

	// Fair series: denominator excludes pairs whose target wasn't in
	// the embedded subset — measures "how well does the model rank
	// targets that actually exist among the embedded docs?"
	s.FairN = s.N - s.NotInCorpus
	if s.FairN > 0 {
		fdenom := float64(s.FairN)
		s.FairAt1 = hits1 / fdenom
		s.FairAt5 = hits5 / fdenom
		s.FairAt10 = hits10 / fdenom
		s.FairMRR = sumRR / fdenom
	}
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
