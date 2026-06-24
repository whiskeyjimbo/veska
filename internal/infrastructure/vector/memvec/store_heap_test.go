// SPDX-License-Identifier: AGPL-3.0-only

package memvec_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"
)

// bruteForceTopK is an independent reference: score every vector, sort ascending
// by squared L2 distance, take k. The bounded-heap Search must agree with it.
func bruteForceTopK(rows []domain.EmbeddingRow, query []float32, k int) []string {
	type c struct {
		id   string
		dist float64
	}
	cands := make([]c, 0, len(rows))
	for _, r := range rows {
		var sum float64
		for i := range query {
			d := float64(r.Vector[i]) - float64(query[i])
			sum += d * d
		}
		cands = append(cands, c{id: r.NodeID, dist: sum})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	if k > len(cands) {
		k = len(cands)
	}
	ids := make([]string, k)
	for i := 0; i < k; i++ {
		ids[i] = cands[i].id
	}
	return ids
}

func hitIDs(hits []domain.SearchHit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.NodeID
	}
	return ids
}

// TestSearchParityWithBruteForce pins that the bounded-heap Search returns the
// same ranked top-k as a full collect-and-sort reference, across a range of k.
// Vectors are drawn so distances are distinct (no tie ambiguity at the cut).
func TestSearchParityWithBruteForce(t *testing.T) {
	const dim = 16
	// Span the parallelScanMin (2048) boundary: n=2000 exercises the in-place
	// sequential scan, n=3000 exercises the sharded parallel scan + merge. Both
	// must match a brute-force top-k exactly.
	for _, n := range []int{2000, 3000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			rows := make([]domain.EmbeddingRow, n)
			for i := range rows {
				v := make([]float32, dim)
				for d := range v {
					v[d] = rng.Float32()
				}
				rows[i] = domain.EmbeddingRow{
					NodeID:      fmt.Sprintf("n%04d", i),
					Vector:      v,
					ContentHash: "h",
					ModelID:     testModel,
				}
			}

			s := memvec.New()
			if err := s.UpsertEmbeddings(context.Background(), testRepo, testBranch, rows); err != nil {
				t.Fatalf("UpsertEmbeddings: %v", err)
			}

			query := make([]float32, dim)
			for d := range query {
				query[d] = rng.Float32()
			}

			// Small k: assert exact rank order against brute force, validating the
			// parallel merge (n=3000 takes the sharded path). Seed 42 keeps the
			// top-k distances well-separated so float32 (l2sq) vs float64 (brute
			// force) rounding can't reorder them; deep-rank order IS precision-
			// dependent, which is why the k>=n case below is a set-check.
			for _, k := range []int{1, 5, 10, 50} {
				want := bruteForceTopK(rows, query, k)
				hits, err := s.Search(context.Background(), testRepo, testBranch, query, k, domain.VectorFilter{})
				if err != nil {
					t.Fatalf("k=%d Search: %v", k, err)
				}
				got := hitIDs(hits)
				if len(got) != len(want) {
					t.Fatalf("k=%d: got %d hits, want %d", k, len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("k=%d: rank %d = %q, want %q\n got=%v\nwant=%v", k, i, got[i], want[i], got, want)
					}
				}
			}

			// Full corpus (k >= n): exact deep-rank order is dominated by
			// float32 (l2sq) vs float64 (brute force) rounding on near-equal
			// distances, so assert the returned SET is complete and the scores
			// are non-increasing rather than a brittle position-by-position match.
			for _, k := range []int{n, n + 100} {
				hits, err := s.Search(context.Background(), testRepo, testBranch, query, k, domain.VectorFilter{})
				if err != nil {
					t.Fatalf("k=%d Search: %v", k, err)
				}
				if len(hits) != n {
					t.Fatalf("k=%d: got %d hits, want all %d rows", k, len(hits), n)
				}
				seen := make(map[string]bool, n)
				for i, h := range hits {
					seen[h.NodeID] = true
					if i > 0 && hits[i].Score > hits[i-1].Score {
						t.Fatalf("k=%d: scores not non-increasing at rank %d (%.6f > %.6f)", k, i, hits[i].Score, hits[i-1].Score)
					}
				}
				if len(seen) != n {
					t.Fatalf("k=%d: returned %d distinct nodes, want %d (set incomplete)", k, len(seen), n)
				}
			}
		})
	}
}

// TestSearchKNonPositive verifies k<=0 returns no hits (and does not panic).
func TestSearchKNonPositive(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, []domain.EmbeddingRow{
		makeRow("n1", vec(1, 0)),
	}); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}
	for _, k := range []int{0, -1, -10} {
		hits, err := s.Search(ctx, testRepo, testBranch, vec(1, 0), k, domain.VectorFilter{})
		if err != nil {
			t.Fatalf("k=%d Search: %v", k, err)
		}
		if len(hits) != 0 {
			t.Fatalf("k=%d: expected 0 hits, got %d", k, len(hits))
		}
	}
}

// TestSearchMergesPartitions verifies that with an empty ModelID filter the top-k
// is computed across every model partition for the repo/branch, not per-partition.
func TestSearchMergesPartitions(t *testing.T) {
	s := memvec.New()
	ctx := context.Background()
	rowA := domain.EmbeddingRow{NodeID: "a", Vector: vec(1, 0), ContentHash: "ha", ModelID: "model-a"}
	rowB := domain.EmbeddingRow{NodeID: "b", Vector: vec(0, 1), ContentHash: "hb", ModelID: "model-b"}
	if err := s.UpsertEmbeddings(ctx, testRepo, testBranch, []domain.EmbeddingRow{rowA, rowB}); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}
	// Query closest to rowB's partition; with merge the top hit must be "b".
	hits, err := s.Search(ctx, testRepo, testBranch, vec(0, 1), 2, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 merged hits, got %d", len(hits))
	}
	if hits[0].NodeID != "b" {
		t.Fatalf("expected top hit 'b' (cross-partition merge), got %q", hits[0].NodeID)
	}
}

// TestSearchAllocsIndependentOfN is the regression guard for O(k) query allocation: the
// per-query allocation must be O(k), not O(N). Allocs/op for a large corpus must
// not exceed those for a small corpus (both bounded by the heap + result slice).
func TestSearchAllocsIndependentOfN(t *testing.T) {
	const dim, k = 32, 10
	build := func(n int) (*memvec.Store, []float32) {
		rng := rand.New(rand.NewSource(7))
		rows := make([]domain.EmbeddingRow, n)
		for i := range rows {
			v := make([]float32, dim)
			for d := range v {
				v[d] = rng.Float32()
			}
			rows[i] = domain.EmbeddingRow{NodeID: fmt.Sprintf("n%d", i), Vector: v, ContentHash: "h", ModelID: testModel}
		}
		s := memvec.New()
		if err := s.UpsertEmbeddings(context.Background(), testRepo, testBranch, rows); err != nil {
			t.Fatalf("UpsertEmbeddings: %v", err)
		}
		q := make([]float32, dim)
		for d := range q {
			q[d] = rng.Float32()
		}
		return s, q
	}

	measure := func(n int) float64 {
		s, q := build(n)
		return testing.AllocsPerRun(50, func() {
			_, _ = s.Search(context.Background(), testRepo, testBranch, q, k, domain.VectorFilter{})
		})
	}

	// Allocs must be O(k), not O(N) - flat as the corpus grows. The scan has two
	// regimes split at parallelScanMin (2048): below it the partition maps are
	// scanned in place; above it the rows are materialized once and the scan is
	// sharded across workers, adding a constant (in N) baseline of goroutine +
	// per-shard-heap allocations. The baselines differ, so independence-of-N is
	// asserted WITHIN each regime rather than across the threshold.
	t.Run("sequential", func(t *testing.T) {
		small, big := measure(300), measure(1800) // both < 2048
		if big > small {
			t.Fatalf("sequential allocs grew with N: 300 -> %.0f, 1800 -> %.0f", small, big)
		}
		t.Logf("sequential allocs/op: N=300 -> %.0f, N=1800 -> %.0f (k=%d)", small, big, k)
	})
	t.Run("parallel", func(t *testing.T) {
		small, big := measure(4000), measure(50_000) // both >= 2048
		if big > small {
			t.Fatalf("parallel allocs grew with N: 4000 -> %.0f, 50000 -> %.0f", small, big)
		}
		t.Logf("parallel allocs/op: N=4000 -> %.0f, N=50000 -> %.0f (k=%d)", small, big, k)
	})
}

// BenchmarkSearch exercises the hot path at a realistic corpus size.
func BenchmarkSearch(b *testing.B) {
	const n, dim, k = 50_000, 32, 10
	rng := rand.New(rand.NewSource(1))
	rows := make([]domain.EmbeddingRow, n)
	for i := range rows {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()
		}
		rows[i] = domain.EmbeddingRow{NodeID: fmt.Sprintf("n%d", i), Vector: v, ContentHash: "h", ModelID: testModel}
	}
	s := memvec.New()
	if err := s.UpsertEmbeddings(context.Background(), testRepo, testBranch, rows); err != nil {
		b.Fatalf("UpsertEmbeddings: %v", err)
	}
	q := make([]float32, dim)
	for d := range q {
		q[d] = rng.Float32()
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Search(context.Background(), testRepo, testBranch, q, k, domain.VectorFilter{}); err != nil {
			b.Fatal(err)
		}
	}
}
