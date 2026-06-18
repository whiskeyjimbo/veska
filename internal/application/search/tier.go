// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package search

// ScoreTier reports a human-readable confidence band for a single
// result's Score relative to the highest-scoring hit in the same
// result set. Bands: "top" >= 95% of top, "strong" >= 80%, "weak"
// otherwise. moves this from cmd/veska/search.go so both
// the MCP response and the CLI render the same label without the CLI
// duplicating the math.
// IMPORTANT: tiers are RELATIVE to the query's top hit, not absolute.
// A query whose best match is poor still gets a "top" label on its
// least-bad result. Callers needing absolute confidence should pair
// the tier with the raw RRF Score and the WeakTopAbsolute floor.
func ScoreTier(s, top float32) string {
	if top <= 0 {
		return "weak"
	}
	ratio := s / top
	switch {
	case ratio >= 0.95:
		return "top"
	case ratio >= 0.80:
		return "strong"
	default:
		return "weak"
	}
}

// WeakTopAbsolute is the post-fusion RRF score floor below which a
// query's top hit is considered weak in absolute terms (the cross
// corroboration signal between vector + lexical is missing). RRF top
// scores cluster ~0.0164 (one list, rank 1) to 0.0328 (both lists,
// rank 1); 0.018 sits a hair above the single-list rank-1 floor so a
// query that only landed in one retriever's list trips the warning.
const WeakTopAbsolute float32 = 0.018

// NormalizeScores returns the per-result min-max-normalised score
// (0.1) for a results slice. When all scores are equal (or only one
// result is present), each entry returns 1.0 so consumers don't have
// to special-case the divide-by-zero. The function is pure - it
// computes a parallel slice instead of mutating results - so the raw
// RRF score remains intact for power consumers.
func NormalizeScores(results []Result) []float32 {
	out := make([]float32, len(results))
	if len(results) == 0 {
		return out
	}
	minS, maxS := results[0].Score, results[0].Score
	for _, r := range results {
		if r.Score < minS {
			minS = r.Score
		}
		if r.Score > maxS {
			maxS = r.Score
		}
	}
	span := maxS - minS
	for i, r := range results {
		if span <= 0 {
			out[i] = 1.0
			continue
		}
		out[i] = (r.Score - minS) / span
	}
	return out
}
