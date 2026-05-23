// Package condense implements extractive condensation: given a list of
// text pieces (lines from a function body, sentences from a doc-comment
// paragraph, etc.), it returns the k most-central pieces in original
// order. Callers can embed the concatenation of the result instead of
// the raw input, which suppresses boilerplate (low-centrality lines
// like repeated `if err != nil { ... }` returns) and concentrates the
// node's vector on signal-dense content.
//
// Scope (solov2-oo4q.1): self-contained Tier 1 helper, no production
// wiring. Tier 2 (small distilled LLM) and Tier 3 (full LLM) live in
// separate sub-beads if the bench at oo4q.2 shows the extractive lift
// is worth chasing further.
//
// Centrality scoring is raw cosine-centroid: each piece's score is the
// average dot product against every other piece (vectors are assumed
// L2-normalised, which model2vec already guarantees, so dot == cosine).
// Chosen over TextRank because piece counts in real veska nodes are
// small (~5-100) — full TextRank's eigenvector iteration is overkill
// at that scale. If quality plateaus during oo4q.2 a future
// implementer can A/B against TextRank here.
package condense

import (
	"context"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Condense embeds each piece with the supplied EmbeddingProvider, scores
// every piece by centrality (mean cosine similarity to all other
// pieces), and returns the top-k pieces in their ORIGINAL order. The
// ordering preservation matters: downstream callers concatenate the
// result and embed it, and natural ordering reads better than
// score-ordering for both code (signature before body) and prose
// (topic sentence before elaboration).
//
// Edge cases:
//   - k <= 0: returns nil (caller asked for nothing).
//   - k >= len(pieces): returns a copy of pieces unchanged (already
//     "fully condensed").
//   - len(pieces) <= 1: returns a copy unchanged (centrality
//     undefined with fewer than 2 pieces).
//   - An empty piece embeds normally; the embedder is responsible for
//     producing a well-defined vector for empty input.
//
// Returns an error if any embed call fails. Partial results are not
// returned — the caller can fall back to the raw input.
func Condense(ctx context.Context, embedder ports.EmbeddingProvider, pieces []string, k int) ([]string, error) {
	if k <= 0 {
		return nil, nil
	}
	if len(pieces) <= 1 || k >= len(pieces) {
		out := make([]string, len(pieces))
		copy(out, pieces)
		return out, nil
	}

	vecs := make([][]float32, len(pieces))
	for i, p := range pieces {
		v, err := embedder.Embed(ctx, p)
		if err != nil {
			return nil, err
		}
		vecs[i] = v
	}

	// Centrality = mean dot product against every other piece. The
	// (n-1) denominator is the same for all pieces so it doesn't
	// affect ranking, but we keep it so the score is a well-defined
	// mean (handy if a future caller wants to threshold-prune
	// low-centrality pieces rather than top-K-select).
	type scored struct {
		idx   int
		score float64
	}
	scores := make([]scored, len(pieces))
	for i := range pieces {
		var sum float64
		for j := range pieces {
			if i == j {
				continue
			}
			sum += float64(dot(vecs[i], vecs[j]))
		}
		scores[i] = scored{idx: i, score: sum / float64(len(pieces)-1)}
	}

	// Pick top-k by score. Stable: ties broken by original index, so
	// for identical pieces the earlier one wins (deterministic).
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		return scores[i].idx < scores[j].idx
	})
	keep := make(map[int]bool, k)
	for i := 0; i < k; i++ {
		keep[scores[i].idx] = true
	}

	out := make([]string, 0, k)
	for i, p := range pieces {
		if keep[i] {
			out = append(out, p)
		}
	}
	return out, nil
}

func dot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
