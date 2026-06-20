// SPDX-License-Identifier: AGPL-3.0-only

package memvec

import (
	"math"
	"math/rand"
	"testing"
)

// l2sqRef is the high-precision float64 reference (the pre-optimization formula).
func l2sqRef(a, b []float32) float64 {
	n := min(len(a), len(b))
	var sum float64
	for i := range n {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

// TestL2sqMatchesReference checks the unrolled float32 l2sq stays within a small
// relative tolerance of the float64 reference across realistic embedding dims and
// over non-multiple-of-4 lengths (so the scalar tail is exercised).
func TestL2sqMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for _, dim := range []int{1, 2, 3, 4, 5, 7, 8, 15, 16, 31, 64, 256, 768, 769} {
		for range 20 {
			a := make([]float32, dim)
			b := make([]float32, dim)
			for i := range a {
				// Centered values in [-1,1], the scale of L2-normalized dims.
				a[i] = rng.Float32()*2 - 1
				b[i] = rng.Float32()*2 - 1
			}
			got := float64(l2sq(a, b))
			want := l2sqRef(a, b)
			// float32 accumulation over up to 768 terms: relative error ~1e-4.
			tol := 1e-4 * (want + 1e-6)
			if math.Abs(got-want) > tol {
				t.Fatalf("dim=%d: l2sq=%v ref=%v |diff|=%v > tol=%v", dim, got, want, math.Abs(got-want), tol)
			}
		}
	}
}

// TestL2sqDifferingLengths pins the documented contract: comparison is bounded by
// the length of the shorter vector.
func TestL2sqDifferingLengths(t *testing.T) {
	a := []float32{1, 2, 3, 4, 5}
	b := []float32{1, 2, 9} // only first 3 compared; diff at index 2 -> (3-9)^2 = 36
	if got := l2sq(a, b); got != 36 {
		t.Fatalf("l2sq bounded by shorter len: got %v want 36", got)
	}
	if got := l2sq(b, a); got != 36 {
		t.Fatalf("l2sq is symmetric in length bound: got %v want 36", got)
	}
	if got := l2sq(nil, a); got != 0 {
		t.Fatalf("l2sq of empty vs non-empty: got %v want 0", got)
	}
}

func benchVecs(dim int) ([]float32, []float32) {
	rng := rand.New(rand.NewSource(1))
	a := make([]float32, dim)
	b := make([]float32, dim)
	for i := range a {
		a[i] = rng.Float32()
		b[i] = rng.Float32()
	}
	return a, b
}

// BenchmarkL2sq measures the per-call cost at a typical embedding dimension.
func BenchmarkL2sq(b *testing.B) {
	a, c := benchVecs(256)
	var sink float32
	b.ReportAllocs()
	for b.Loop() {
		sink = l2sq(a, c)
	}
	_ = sink
}
