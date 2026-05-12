// Package eval defines the VectorIndex interface and shared harness utilities
// for evaluating HNSW backing library candidates (usearch, coder/hnsw, lancedb).
//
// All adapters must implement VectorIndex. The harness measures:
//   - recall@10 using brute-force ground truth
//   - warm p95 query latency at k=10
//   - backup round-trip correctness (Save → Load → same results)
package eval

import (
	"math"
	"slices"
	"sort"
	"time"
)

// VectorIndex is the common interface all HNSW backing adapters must satisfy.
type VectorIndex interface {
	// Add inserts a vector with the given 0-based id.
	Add(id uint64, vec []float32) error
	// Search returns the k nearest ids for the query vector.
	// Returned ids match the ids passed to Add.
	Search(query []float32, k int) ([]uint64, error)
	// Save persists the index to the given file path (or directory for lancedb).
	Save(path string) error
	// Load restores the index from the given path.
	Load(path string) error
	// Len returns the number of vectors currently in the index.
	Len() int
}

// RecallResult holds per-population evaluation results.
type RecallResult struct {
	Population int
	RecallAt10 float64
	P95Ms      float64 // warm p95 query latency in milliseconds
	HoldOuts   int
}

// l2sq returns the squared Euclidean distance between two vectors.
func l2sq(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

// BruteForceKNN returns the k nearest 0-based indices from corpus for the query.
func BruteForceKNN(corpus [][]float32, query []float32, k int) []uint64 {
	type entry struct {
		idx  uint64
		dist float64
	}
	entries := make([]entry, len(corpus))
	for i, v := range corpus {
		entries[i] = entry{idx: uint64(i), dist: l2sq(v, query)}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.dist < b.dist {
			return -1
		}
		if a.dist > b.dist {
			return 1
		}
		return 0
	})
	limit := min(k, len(entries))
	out := make([]uint64, limit)
	for i := range limit {
		out[i] = entries[i].idx
	}
	return out
}

// ComputeRecall returns the fraction of groundTruth ids found in returned ids.
func ComputeRecall(groundTruth, returned []uint64) float64 {
	if len(groundTruth) == 0 {
		return 0
	}
	set := make(map[uint64]struct{}, len(returned))
	for _, id := range returned {
		set[id] = struct{}{}
	}
	var hits int
	for _, id := range groundTruth {
		if _, ok := set[id]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(groundTruth))
}

// MeasureRecallAndLatency runs nQueries warm queries against idx, computing
// recall@10 against brute-force ground truth on corpus, and returns the
// RecallResult with p95 latency. holdOut vectors must not be in the corpus.
func MeasureRecallAndLatency(idx VectorIndex, corpus [][]float32, holdOut [][]float32) RecallResult {
	const k = 10
	latencies := make([]float64, len(holdOut))
	var sumRecall float64

	for i, q := range holdOut {
		gt := BruteForceKNN(corpus, q, k)
		t0 := time.Now()
		ret, err := idx.Search(q, k)
		latencies[i] = float64(time.Since(t0).Nanoseconds()) / 1e6
		if err != nil {
			continue
		}
		sumRecall += ComputeRecall(gt, ret)
	}

	sort.Float64s(latencies)
	p95Idx := max(int(math.Ceil(float64(len(latencies))*0.95))-1, 0)

	return RecallResult{
		Population: idx.Len(),
		RecallAt10: sumRecall / float64(len(holdOut)),
		P95Ms:      latencies[p95Idx],
		HoldOuts:   len(holdOut),
	}
}
