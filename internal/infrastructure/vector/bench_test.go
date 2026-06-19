// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/recall"
)

const (
	benchRepoID  = "bench"
	benchBranch  = "main"
	benchModelID = "nomic-embed-text"
	corpusSeed   = 42
	holdOutSeed  = 999
	nHoldOut     = 100
	nWarmQueries = 200
	batchSize    = 1000
)

// buildStore constructs a temporary, populated UsearchStore containing n randomly
// generated vectors for performance evaluation.
func buildStore(tb testing.TB, n int) (*vector.UsearchStore, [][]float32) {
	tb.Helper()
	store, err := vector.NewUsearchStore()
	if err != nil {
		tb.Fatalf("NewUsearchStore: %v", err)
	}
	tb.Cleanup(func() { store.Destroy() })

	corpus := gen.GenerateVectors(n, corpusSeed)
	ctx := context.Background()

	for start := 0; start < n; start += batchSize {
		end := start + batchSize
		if end > n {
			end = n
		}
		batch := make([]domain.EmbeddingRow, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, domain.EmbeddingRow{
				NodeID:      fmt.Sprintf("node-%d", i),
				ContentHash: fmt.Sprintf("hash-%d", i),
				ModelID:     benchModelID,
				Vector:      corpus[i],
			})
		}
		if err := store.UpsertEmbeddings(ctx, benchRepoID, benchBranch, batch); err != nil {
			tb.Fatalf("UpsertEmbeddings batch starting at %d: %v", start, err)
		}
	}

	return store, corpus
}

// computeRecallAt10 calculates the average recall at a retrieval limit of 10 for
// query vectors, comparing search results against brute-force ground truth.
func computeRecallAt10(tb testing.TB, store *vector.UsearchStore, corpus [][]float32) float64 {
	tb.Helper()
	holdOut := gen.GenerateVectors(nHoldOut, holdOutSeed)
	ctx := context.Background()
	filter := domain.VectorFilter{ModelID: benchModelID}

	var sumRecall float64
	for _, q := range holdOut {
		// Brute-force ground truth (1-indexed rowids from recall.BruteForceKNN).
		gt1 := recall.BruteForceKNN(corpus, q, 10)

		hits, err := store.Search(ctx, benchRepoID, benchBranch, q, 10, filter)
		if err != nil {
			tb.Fatalf("Search: %v", err)
		}

		// Map node ID strings back to 1-based indices because the brute-force
		// ground truth implementation operates on 1-based vector indices.
		ret1 := make([]int64, 0, len(hits))
		for _, h := range hits {
			var idx int
			_, parseErr := fmt.Sscanf(h.NodeID, "node-%d", &idx)
			if parseErr != nil {
				continue
			}
			ret1 = append(ret1, int64(idx+1))
		}

		sumRecall += recall.ComputeRecall(gt1, ret1)
	}
	return sumRecall / float64(nHoldOut)
}

func TestRecallFloor50k(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 50k recall test in short mode")
	}
	store, corpus := buildStore(t, 50_000)
	got := computeRecallAt10(t, store, corpus)
	const floor = 0.95
	t.Logf("recall@10 @50k = %.4f (floor %.2f)", got, floor)
	if got < floor {
		t.Errorf("recall@10 @50k = %.4f < floor %.2f", got, floor)
	}
}

func TestRecallFloor250k(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 250k recall test in short mode")
	}
	store, corpus := buildStore(t, 250_000)
	got := computeRecallAt10(t, store, corpus)
	const floor = 0.85
	t.Logf("recall@10 @250k = %.4f (floor %.2f)", got, floor)
	if got < floor {
		t.Errorf("recall@10 @250k = %.4f < floor %.2f", got, floor)
	}
}

// BenchmarkSearch50k measures warm search performance under load on 50k vectors.
func BenchmarkSearch50k(b *testing.B) {
	store, _ := buildStore(b, 50_000)
	warmQueries := gen.GenerateVectors(nWarmQueries, 7777)
	ctx := context.Background()
	filter := domain.VectorFilter{ModelID: benchModelID}

	// Warm up the search cache to measure steady-state query latencies.
	for _, q := range warmQueries {
		_, _ = store.Search(ctx, benchRepoID, benchBranch, q, 10, filter)
	}

	queries := gen.GenerateVectors(nWarmQueries, holdOutSeed)
	latencies := make([]float64, nWarmQueries)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := queries[i%nWarmQueries]
		t0 := timeNow()
		_, _ = store.Search(ctx, benchRepoID, benchBranch, q, 10, filter)
		latencies[i%nWarmQueries] = float64(timeNow()-t0) / 1e6
	}
	b.StopTimer()

	sort.Float64s(latencies)
	p95Idx := max(int(math.Ceil(float64(len(latencies))*0.95))-1, 0)
	b.ReportMetric(latencies[p95Idx], "p95ms")

	const p95Budget = 100.0
	if latencies[p95Idx] > p95Budget {
		b.Errorf("p95 = %.2fms > %.0fms budget @50k", latencies[p95Idx], p95Budget)
	}
}

// BenchmarkSearch250k measures warm search performance under load on 250k vectors.
func BenchmarkSearch250k(b *testing.B) {
	store, _ := buildStore(b, 250_000)
	warmQueries := gen.GenerateVectors(nWarmQueries, 7777)
	ctx := context.Background()
	filter := domain.VectorFilter{ModelID: benchModelID}

	// Warm up the search cache to measure steady-state query latencies.
	for _, q := range warmQueries {
		_, _ = store.Search(ctx, benchRepoID, benchBranch, q, 10, filter)
	}

	queries := gen.GenerateVectors(nWarmQueries, holdOutSeed)
	latencies := make([]float64, nWarmQueries)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := queries[i%nWarmQueries]
		t0 := timeNow()
		_, _ = store.Search(ctx, benchRepoID, benchBranch, q, 10, filter)
		latencies[i%nWarmQueries] = float64(timeNow()-t0) / 1e6
	}
	b.StopTimer()

	sort.Float64s(latencies)
	p95Idx := max(int(math.Ceil(float64(len(latencies))*0.95))-1, 0)
	b.ReportMetric(latencies[p95Idx], "p95ms")

	const p95Budget = 150.0
	if latencies[p95Idx] > p95Budget {
		b.Errorf("p95 = %.2fms > %.0fms budget @250k", latencies[p95Idx], p95Budget)
	}
}
