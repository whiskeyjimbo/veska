// SPDX-License-Identifier: AGPL-3.0-only

package veccodec

import (
	"encoding/binary"
	"math"
	"testing"
)

func encode(vec []float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, f := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func TestDecodeFloat32LE_RoundTrip(t *testing.T) {
	want := []float32{0, 1.5, -2.25, 3.0e9}
	got := DecodeFloat32LE(encode(want), len(want))
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestDecodeFloat32LE_ShortBlobTruncates(t *testing.T) {
	// A blob holding only 2 floats with dim=4 must truncate to 2, not panic.
	blob := encode([]float32{1, 2})
	got := DecodeFloat32LE(blob, 4)
	if len(got) != 2 {
		t.Fatalf("short blob: len = %d, want 2 (truncated)", len(got))
	}
}

func TestDecodeFloat32LE_EmptyBlob(t *testing.T) {
	if got := DecodeFloat32LE(nil, 4); len(got) != 0 {
		t.Fatalf("nil blob: len = %d, want 0", len(got))
	}
}
