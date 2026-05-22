// Package recall contains the eval harness for veska's semantic-search
// service. The pure recall@k / p95 computations live here without a build
// tag so they compile under default `go test`/`go vet` and can be unit-
// tested in isolation. The end-to-end eval driver that wires up a real
// search.Service is gated by the `eval` build tag in recall_test.go.
package recall

import (
	"math"
	"slices"
	"time"
)

// RecallAtK returns the recall@k for a single query.
//
// hits is the ordered list of NodeIDs the search service returned (ranked
// best first); truth is the set of NodeIDs that are correct answers for
// the query. The returned value is len(hits[:min(k,len(hits))] ∩ truth)
// divided by min(k, len(truth)). When len(truth) is zero the result is
// zero — a query with no correct answers cannot have positive recall.
//
// Semantics note: we normalise the denominator by min(k, len(truth))
// rather than len(truth). This is the standard recall@k definition: if
// truth has more members than k the score is bounded by 1.0 (otherwise
// recall would be capped below 1 even with a perfect ranker).
func RecallAtK(hits []string, truth map[string]struct{}, k int) float64 {
	if k <= 0 || len(truth) == 0 || len(hits) == 0 {
		return 0
	}
	upper := min(len(hits), k)
	matched := 0
	for i := range upper {
		if _, ok := truth[hits[i]]; ok {
			matched++
		}
	}
	denom := min(len(truth), k)
	return float64(matched) / float64(denom)
}

// MeanRecall returns the arithmetic mean of per-query recall values.
// An empty input returns zero.
func MeanRecall(perQuery []float64) float64 {
	if len(perQuery) == 0 {
		return 0
	}
	var sum float64
	for _, r := range perQuery {
		sum += r
	}
	return sum / float64(len(perQuery))
}

// P95Latency returns the 95th-percentile latency from the supplied
// durations using nearest-rank interpolation. An empty slice returns
// zero; a single-element slice returns that element.
func P95Latency(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	slices.Sort(sorted)
	// Nearest-rank: ceil(0.95 * N) - 1 (0-indexed).
	rank := max(int(math.Ceil(0.95*float64(len(sorted))))-1, 0)
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
