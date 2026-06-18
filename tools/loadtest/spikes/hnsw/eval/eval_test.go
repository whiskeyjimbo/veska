// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package eval_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/hnsw/eval"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
)

// TestBruteForceKNN verifies nearest-neighbor correctness on a tiny hand-crafted corpus.
func TestBruteForceKNN(t *testing.T) {
	// 3-dim corpus for simplicity; point 1 is the nearest to query.
	corpus := [][]float32{
		{10, 10, 10},
		{1, 1, 1}, // nearest to {1.1, 1.1, 1.1}
		{5, 5, 5},
	}
	query := []float32{1.1, 1.1, 1.1}
	nn := eval.BruteForceKNN(corpus, query, 1)
	if len(nn) != 1 {
		t.Fatalf("expected 1 result, got %d", len(nn))
	}
	if nn[0] != 1 {
		t.Errorf("expected id 1, got %d", nn[0])
	}
}

// TestComputeRecall verifies the recall computation formula.
func TestComputeRecall(t *testing.T) {
	tests := []struct {
		name string
		gt   []uint64
		ret  []uint64
		want float64
	}{
		{"perfect", []uint64{0, 1, 2}, []uint64{0, 1, 2}, 1.0},
		{"half", []uint64{0, 1, 2, 3}, []uint64{0, 1, 99, 100}, 0.5},
		{"none", []uint64{0, 1}, []uint64{2, 3}, 0.0},
		{"empty gt", []uint64{}, []uint64{1, 2}, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eval.ComputeRecall(tc.gt, tc.ret)
			if abs(got-tc.want) > 1e-9 {
				t.Errorf("got %.4f, want %.4f", got, tc.want)
			}
		})
	}
}

// TestMeasureRecallAndLatency verifies the harness plumbing via a stub index.
func TestMeasureRecallAndLatency(t *testing.T) {
	// Perfect stub index: returns the actual brute-force neighbours.
	corpus := gen.GenerateVectors(200, 42)
	holdOut := gen.GenerateVectors(20, 999)

	stub := &stubIndex{corpus: corpus}
	result := eval.MeasureRecallAndLatency(stub, corpus, holdOut)

	if result.Population != len(corpus) {
		t.Errorf("Population: got %d, want %d", result.Population, len(corpus))
	}
	if result.HoldOuts != len(holdOut) {
		t.Errorf("HoldOuts: got %d, want %d", result.HoldOuts, len(holdOut))
	}
	// Perfect stub → recall must be 1.0.
	if abs(result.RecallAt10-1.0) > 1e-9 {
		t.Errorf("RecallAt10: got %.4f, want 1.0", result.RecallAt10)
	}
	if result.P95Ms < 0 {
		t.Errorf("P95Ms must be non-negative")
	}
}

// stubIndex is a perfect VectorIndex that uses brute-force for Search.
type stubIndex struct {
	corpus [][]float32
}

func (s *stubIndex) Add(_ uint64, _ []float32) error { return nil }
func (s *stubIndex) Save(_ string) error             { return nil }
func (s *stubIndex) Load(_ string) error             { return nil }
func (s *stubIndex) Len() int                        { return len(s.corpus) }
func (s *stubIndex) Search(q []float32, k int) ([]uint64, error) {
	return eval.BruteForceKNN(s.corpus, q, k), nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
