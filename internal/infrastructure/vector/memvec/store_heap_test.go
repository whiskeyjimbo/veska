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
	const (
		n   = 2000
		dim = 16
	)
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

	for _, k := range []int{1, 5, 10, 50, n, n + 100} {
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

// TestSearchAllocsIndependentOfN is the regression guard for solov2-jncy: the
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

	small := measure(500)
	big := measure(50_000)
	// O(k) means big must not allocate more than small (allow no growth with N).
	if big > small {
		t.Fatalf("allocs grew with corpus size: N=500 -> %.0f allocs, N=50000 -> %.0f allocs", small, big)
	}
	t.Logf("allocs/op: N=500 -> %.0f, N=50000 -> %.0f (k=%d)", small, big, k)
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
