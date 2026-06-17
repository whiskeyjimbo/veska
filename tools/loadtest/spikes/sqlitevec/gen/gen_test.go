// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package gen_test

import (
	"math"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
)

const dim = 768

func l2Norm(v []float32) float64 {
	var s float64
	for _, f := range v {
		s += float64(f) * float64(f)
	}
	return math.Sqrt(s)
}

func TestGenerateVectorsDimension(t *testing.T) {
	vecs := gen.GenerateVectors(100, 42)
	if len(vecs) != 100 {
		t.Fatalf("count: got %d, want 100", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != dim {
			t.Errorf("vec[%d]: dim %d, want %d", i, len(v), dim)
		}
	}
}

func TestGenerateVectorsNormDistribution(t *testing.T) {
	n := 2000
	vecs := gen.GenerateVectors(n, 12345)

	norms := make([]float64, n)
	var sum float64
	for i, v := range vecs {
		norms[i] = l2Norm(v)
		sum += norms[i]
	}

	mean := sum / float64(n)
	if mean < 6.5 || mean > 8.5 {
		t.Errorf("mean norm %f out of expected range [6.5, 8.5]", mean)
	}

	sort.Float64s(norms)

	p50 := norms[n/2]
	if p50 < 6.0 || p50 > 9.0 {
		t.Errorf("p50 norm %f out of expected range [6.0, 9.0]", p50)
	}

	p95 := norms[int(float64(n)*0.95)]
	if p95 < 8.0 || p95 > 12.0 {
		t.Errorf("p95 norm %f out of expected range [8.0, 12.0]", p95)
	}
}

func TestComputeStats(t *testing.T) {
	vecs := gen.GenerateVectors(500, 99)
	stats := gen.ComputeStats(vecs)

	if stats.Mean < 6.0 || stats.Mean > 9.0 {
		t.Errorf("stats.Mean %f out of range", stats.Mean)
	}
	if stats.P50 < 5.5 || stats.P50 > 10.0 {
		t.Errorf("stats.P50 %f out of range", stats.P50)
	}
	if stats.P95 < 7.0 || stats.P95 > 13.0 {
		t.Errorf("stats.P95 %f out of range", stats.P95)
	}
}
