package autolink

import (
	"math"
	"testing"
)

func TestFPRate_Empty(t *testing.T) {
	t.Parallel()
	if got := FPRate(nil); got != 0 {
		t.Fatalf("FPRate(nil): want 0, got %v", got)
	}
	if got := FPRate([]Pair{}); got != 0 {
		t.Fatalf("FPRate(empty): want 0, got %v", got)
	}
}

// 3 TP + 1 FP among 4 candidates → 0.25.
func TestFPRate_MixedPartial(t *testing.T) {
	t.Parallel()
	pairs := []Pair{
		{TruePositive: true},
		{TruePositive: true},
		{TruePositive: true},
		{TruePositive: false},
	}
	got := FPRate(pairs)
	want := 0.25
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("FPRate 3TP/1FP: want %v, got %v", want, got)
	}
	fp, tp := FPCounts(pairs)
	if fp != 1 || tp != 3 {
		t.Fatalf("FPCounts: want (1,3), got (%d,%d)", fp, tp)
	}
}

func TestFPRate_AllTP(t *testing.T) {
	t.Parallel()
	pairs := []Pair{{TruePositive: true}, {TruePositive: true}}
	if got := FPRate(pairs); got != 0 {
		t.Fatalf("all-TP: want 0, got %v", got)
	}
}

func TestFPRate_AllFP(t *testing.T) {
	t.Parallel()
	pairs := []Pair{{TruePositive: false}, {TruePositive: false}, {TruePositive: false}}
	if got := FPRate(pairs); got != 1.0 {
		t.Fatalf("all-FP: want 1.0, got %v", got)
	}
}

func TestFPRate_SingleFP(t *testing.T) {
	t.Parallel()
	pairs := []Pair{{TruePositive: false}}
	if got := FPRate(pairs); got != 1.0 {
		t.Fatalf("single FP: want 1.0, got %v", got)
	}
}
