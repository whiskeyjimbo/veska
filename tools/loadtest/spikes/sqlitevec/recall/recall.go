// SPDX-License-Identifier: AGPL-3.0-only

// Package recall provides hold-out generation and recall@k computation for sqlite-vec KNN queries.
// It measures how accurately vec0's approximate KNN matches brute-force exact L2 nearest neighbors.
package recall

import (
	"database/sql"
	"fmt"
	"math"
	"slices"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/bench"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
)

// RecallResult holds recall@k results for one population.
type RecallResult struct {
	Population  int64   `json:"population"`
	RecallAt10  float64 `json:"recall_at_10"`
	RecallAt50  float64 `json:"recall_at_50"`
	HoldOutSize int     `json:"hold_out_size"`
}

// distEntry is used for sorting corpus vectors by L2 distance to a query.
type distEntry struct {
	rowid int64
	dist  float64
}

// l2Sq computes the squared L2 distance between two float32 vectors.
func l2Sq(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

// BruteForceKNN returns the k nearest neighbor rowids (1-indexed) from corpus
// for the given query vector using L2 distance. rowids[i] = i+1.
// Results are sorted by ascending distance (nearest first).
func BruteForceKNN(corpus [][]float32, query []float32, k int) []int64 {
	entries := make([]distEntry, len(corpus))
	for i, v := range corpus {
		entries[i] = distEntry{
			rowid: int64(i + 1),
			dist:  math.Sqrt(l2Sq(v, query)),
		}
	}
	slices.SortFunc(entries, func(a, b distEntry) int {
		switch {
		case a.dist < b.dist:
			return -1
		case a.dist > b.dist:
			return 1
		default:
			return 0
		}
	})

	limit := min(k, len(entries))
	result := make([]int64, limit)
	for i := range limit {
		result[i] = entries[i].rowid
	}
	return result
}

// ComputeRecall computes recall@k: fraction of groundTruth rowids found in returned rowids.
// len(groundTruth) is used as k. Returns 0 if groundTruth is empty.
func ComputeRecall(groundTruth, returned []int64) float64 {
	if len(groundTruth) == 0 {
		return 0.0
	}

	// Build a set of returned rowids for O(1) lookup.
	retSet := make(map[int64]struct{}, len(returned))
	for _, id := range returned {
		retSet[id] = struct{}{}
	}

	var hits int
	for _, id := range groundTruth {
		if _, ok := retSet[id]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(groundTruth))
}

// GenerateHoldOut returns nQueries query vectors using the given seed (deterministic).
// These are generated independently from the corpus via gen.GenerateVectors.
func GenerateHoldOut(nQueries int, seed uint64) [][]float32 {
	return gen.GenerateVectors(nQueries, seed)
}

// RunRecall loads the corpus into the DB (assumed already loaded via the caller),
// generates ground-truth top-k via brute-force, queries vec0 for each hold-out vector,
// and returns a RecallResult for this population.
// corpus must already be inserted into db's vec_nodes table (rowids 1.len(corpus)).
// holdOut vectors must NOT be in the corpus.
func RunRecall(db *sql.DB, corpus [][]float32, holdOut [][]float32, population int64) (RecallResult, error) {
	const k10 = 10
	const k50 = 50

	var sumRecall10, sumRecall50 float64

	for _, query := range holdOut {
		// Brute-force ground truth.
		gt10 := BruteForceKNN(corpus, query, k10)
		gt50 := BruteForceKNN(corpus, query, k50)

		// vec0 approximate results.
		ret10, err := bench.QueryVec0(db, query, k10)
		if err != nil {
			return RecallResult{}, fmt.Errorf("recall: query k=10: %w", err)
		}
		ret50, err := bench.QueryVec0(db, query, k50)
		if err != nil {
			return RecallResult{}, fmt.Errorf("recall: query k=50: %w", err)
		}

		sumRecall10 += ComputeRecall(gt10, ret10)
		sumRecall50 += ComputeRecall(gt50, ret50)
	}

	n := float64(len(holdOut))
	return RecallResult{
		Population:  population,
		RecallAt10:  sumRecall10 / n,
		RecallAt50:  sumRecall50 / n,
		HoldOutSize: len(holdOut),
	}, nil
}
