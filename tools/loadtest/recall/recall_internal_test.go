package recall

import (
	"math"
	"testing"
	"time"
)

func mkTruth(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

// TestRecallAtK_PerfectHits: all k top hits are in truth and truth size
// is exactly k, so recall is 1.0.
func TestRecallAtK_PerfectHits(t *testing.T) {
	t.Parallel()
	hits := []string{"a", "b", "c"}
	truth := mkTruth("a", "b", "c")
	got := RecallAtK(hits, truth, 3)
	if got != 1.0 {
		t.Fatalf("RecallAtK perfect: want 1.0, got %v", got)
	}
}

// TestRecallAtK_PartialHits: 2 of 3 top hits are in a 3-element truth
// set, so recall = 2/3.
func TestRecallAtK_PartialHits(t *testing.T) {
	t.Parallel()
	hits := []string{"a", "x", "c"}
	truth := mkTruth("a", "b", "c")
	got := RecallAtK(hits, truth, 3)
	want := 2.0 / 3.0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("RecallAtK partial: want %v, got %v", want, got)
	}
}

// TestRecallAtK_TruthLargerThanK: denominator is capped at k so a
// perfect top-k yields 1.0 even when truth is larger than k.
func TestRecallAtK_TruthLargerThanK(t *testing.T) {
	t.Parallel()
	hits := []string{"a", "b", "c"}
	truth := mkTruth("a", "b", "c", "d", "e")
	got := RecallAtK(hits, truth, 3)
	if got != 1.0 {
		t.Fatalf("RecallAtK truth>k: want 1.0, got %v", got)
	}
}

// TestRecallAtK_TruthSmallerThanK: denominator is min(k, len(truth)),
// so a perfect 1-of-1 yields 1.0 even when k=10.
func TestRecallAtK_TruthSmallerThanK(t *testing.T) {
	t.Parallel()
	hits := []string{"a", "x", "y"}
	truth := mkTruth("a")
	got := RecallAtK(hits, truth, 10)
	if got != 1.0 {
		t.Fatalf("RecallAtK truth<k: want 1.0, got %v", got)
	}
}

func TestRecallAtK_NoHits(t *testing.T) {
	t.Parallel()
	if got := RecallAtK([]string{"x", "y"}, mkTruth("a", "b"), 5); got != 0 {
		t.Fatalf("RecallAtK no overlap: want 0, got %v", got)
	}
}

func TestRecallAtK_Edges(t *testing.T) {
	t.Parallel()
	if got := RecallAtK(nil, mkTruth("a"), 5); got != 0 {
		t.Fatalf("nil hits: want 0, got %v", got)
	}
	if got := RecallAtK([]string{"a"}, nil, 5); got != 0 {
		t.Fatalf("nil truth: want 0, got %v", got)
	}
	if got := RecallAtK([]string{"a"}, mkTruth("a"), 0); got != 0 {
		t.Fatalf("k=0: want 0, got %v", got)
	}
}

func TestMeanRecall(t *testing.T) {
	t.Parallel()
	got := MeanRecall([]float64{1.0, 0.5, 0.0})
	want := 0.5
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("MeanRecall: want %v, got %v", want, got)
	}
	if MeanRecall(nil) != 0 {
		t.Fatalf("MeanRecall(nil): want 0")
	}
}

func TestP95Latency(t *testing.T) {
	t.Parallel()
	// 20 samples [1ms..20ms]. ceil(0.95*20) = 19; rank index = 18 → 19ms.
	samples := make([]time.Duration, 20)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}
	got := P95Latency(samples)
	want := 19 * time.Millisecond
	if got != want {
		t.Fatalf("P95Latency: want %v, got %v", want, got)
	}

	if P95Latency(nil) != 0 {
		t.Fatalf("P95Latency(nil): want 0")
	}
	if got := P95Latency([]time.Duration{42 * time.Millisecond}); got != 42*time.Millisecond {
		t.Fatalf("P95Latency single: want 42ms, got %v", got)
	}
}
