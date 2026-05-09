package vecbin_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/sqlitevec/vecbin"
)

const dim = 768

func TestWriteReadRoundTrip(t *testing.T) {
	vecs := [][]float32{
		make([]float32, dim),
		make([]float32, dim),
	}
	for i := range vecs[0] {
		vecs[0][i] = float32(i) * 0.001
		vecs[1][i] = float32(dim-i) * 0.002
	}

	var buf bytes.Buffer
	if err := vecbin.WriteVectors(&buf, vecs); err != nil {
		t.Fatalf("WriteVectors: %v", err)
	}

	got, err := vecbin.ReadVectors(&buf)
	if err != nil {
		t.Fatalf("ReadVectors: %v", err)
	}

	if len(got) != len(vecs) {
		t.Fatalf("count: got %d, want %d", len(got), len(vecs))
	}
	for i, v := range got {
		if len(v) != dim {
			t.Errorf("vec[%d]: dim %d, want %d", i, len(v), dim)
		}
		for j, f := range v {
			if f != vecs[i][j] {
				t.Errorf("vec[%d][%d]: got %f, want %f", i, j, f, vecs[i][j])
			}
		}
	}
}

func TestWriteReadEmptySlice(t *testing.T) {
	var buf bytes.Buffer
	if err := vecbin.WriteVectors(&buf, nil); err != nil {
		t.Fatalf("WriteVectors nil: %v", err)
	}
	got, err := vecbin.ReadVectors(&buf)
	if err != nil {
		t.Fatalf("ReadVectors: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 vectors, got %d", len(got))
	}
}

func TestVectorDimension(t *testing.T) {
	v := make([]float32, dim)
	for i := range v {
		v[i] = 1.0
	}
	var buf bytes.Buffer
	if err := vecbin.WriteVectors(&buf, [][]float32{v}); err != nil {
		t.Fatalf("WriteVectors: %v", err)
	}
	got, err := vecbin.ReadVectors(&buf)
	if err != nil {
		t.Fatalf("ReadVectors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("count: got %d, want 1", len(got))
	}
	if len(got[0]) != dim {
		t.Errorf("dimension: got %d, want %d", len(got[0]), dim)
	}
	var sumSq float64
	for _, f := range got[0] {
		sumSq += float64(f) * float64(f)
	}
	wantNorm := math.Sqrt(float64(dim))
	gotNorm := math.Sqrt(sumSq)
	if math.Abs(gotNorm-wantNorm) > 1e-3 {
		t.Errorf("norm: got %f, want ~%f", gotNorm, wantNorm)
	}
}
