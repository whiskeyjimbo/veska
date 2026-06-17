package search_test

import (
	"math"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/search"
)

func TestScoreTier_TopStrongWeak(t *testing.T) {
	const top float32 = 0.030
	tests := []struct {
		score float32
		want  string
	}{
		{top, "top"},
		{top * 0.95, "top"},
		{top * 0.94999, "strong"},
		{top * 0.80, "strong"},
		{top * 0.799, "weak"},
		{top * 0.50, "weak"},
		{0, "weak"},
	}
	for _, tt := range tests {
		if got := search.ScoreTier(tt.score, top); got != tt.want {
			t.Errorf("ScoreTier(%v, %v) = %q, want %q", tt.score, top, got, tt.want)
		}
	}
}

func TestScoreTier_ZeroTopIsAlwaysWeak(t *testing.T) {
	// when no result has any score (empty set anchor),
	// every hit gets the conservative "weak" label.
	for _, s := range []float32{0, 0.5, 1.0} {
		if got := search.ScoreTier(s, 0); got != "weak" {
			t.Errorf("ScoreTier(%v, 0) = %q, want weak", s, got)
		}
	}
}

func TestNormalizeScores_MinMaxRescaledTo01(t *testing.T) {
	in := []search.Result{
		{Score: 0.010},
		{Score: 0.020},
		{Score: 0.030},
	}
	got := search.NormalizeScores(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if math.Abs(float64(got[0])-0) > 1e-6 {
		t.Errorf("min should normalise to 0, got %v", got[0])
	}
	if math.Abs(float64(got[1])-0.5) > 1e-6 {
		t.Errorf("middle should normalise to 0.5, got %v", got[1])
	}
	if math.Abs(float64(got[2])-1.0) > 1e-6 {
		t.Errorf("max should normalise to 1, got %v", got[2])
	}
}

func TestNormalizeScores_AllEqualScores(t *testing.T) {
	// when the result set has zero spread (all scores
	// equal, or only one result), every hit gets 1.0 - no divide-by
	// zero, and the caller still sees a meaningful "this is the best
	// we've got" signal.
	in := []search.Result{
		{Score: 0.020},
		{Score: 0.020},
	}
	got := search.NormalizeScores(in)
	for i, n := range got {
		if n != 1.0 {
			t.Errorf("equal-score result %d: got %v, want 1.0", i, n)
		}
	}
}

func TestNormalizeScores_EmptyAndSingleResult(t *testing.T) {
	if got := search.NormalizeScores(nil); len(got) != 0 {
		t.Errorf("empty input: len = %d, want 0", len(got))
	}
	got := search.NormalizeScores([]search.Result{{Score: 0.025}})
	if len(got) != 1 || got[0] != 1.0 {
		t.Errorf("single result: got %v, want [1.0]", got)
	}
}
