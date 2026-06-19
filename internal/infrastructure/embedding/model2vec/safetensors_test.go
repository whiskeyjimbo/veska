// SPDX-License-Identifier: AGPL-3.0-only

package model2vec

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// buildSafetensorsFile constructs a minimal in-memory Safetensors payload containing a single float32 tensor.
func buildSafetensorsFile(t *testing.T, name string, shape []int, data []float32) []byte {
	t.Helper()
	// Safetensors data consists of a flat byte buffer described by header offsets.
	dataBytes := make([]byte, 4*len(data))
	for i, v := range data {
		binary.LittleEndian.PutUint32(dataBytes[i*4:], math.Float32bits(v))
	}
	header := `{"` + name + `":{"dtype":"F32","shape":` + intsToJSON(shape) +
		`,"data_offsets":[0,` + intToStr(len(dataBytes)) + `]}}`
	var buf bytes.Buffer
	hdrLen := uint64(len(header))
	_ = binary.Write(&buf, binary.LittleEndian, hdrLen)
	buf.WriteString(header)
	buf.Write(dataBytes)
	return buf.Bytes()
}

func intsToJSON(xs []int) string {
	var out strings.Builder
	out.WriteString("[")
	for i, x := range xs {
		if i > 0 {
			out.WriteString(",")
		}
		out.WriteString(intToStr(x))
	}
	return out.String() + "]"
}

func intToStr(x int) string {
	if x == 0 {
		return "0"
	}
	neg := false
	if x < 0 {
		neg = true
		x = -x
	}
	var buf [20]byte
	i := len(buf)
	for x > 0 {
		i--
		buf[i] = byte('0' + x%10)
		x /= 10
	}
	s := string(buf[i:])
	if neg {
		s = "-" + s
	}
	return s
}

// TestReadSafetensors_RoundTripsSingleF32 verifies that a written single float32 tensor can be read back identically.
func TestReadSafetensors_RoundTripsSingleF32(t *testing.T) {
	in := []float32{1, 2, 3, 4, 5, 6}
	blob := buildSafetensorsFile(t, "embeddings", []int{2, 3}, in)

	tensors, err := readSafetensors(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("readSafetensors: %v", err)
	}
	got, ok := tensors["embeddings"]
	if !ok {
		t.Fatalf("missing 'embeddings' tensor; keys=%v", keysOf(tensors))
	}
	if got.Dtype != "F32" {
		t.Errorf("dtype: got %q, want F32", got.Dtype)
	}
	if len(got.Shape) != 2 || got.Shape[0] != 2 || got.Shape[1] != 3 {
		t.Errorf("shape: got %v, want [2 3]", got.Shape)
	}
	if len(got.Data) != len(in) {
		t.Fatalf("data length: got %d, want %d", len(got.Data), len(in))
	}
	for i := range in {
		if got.Data[i] != in[i] {
			t.Errorf("data[%d]: got %v, want %v", i, got.Data[i], in[i])
		}
	}
}

// TestReadSafetensors_TruncatedHeaderErrors verifies that a truncated Safetensors header triggers an error.
func TestReadSafetensors_TruncatedHeaderErrors(t *testing.T) {
	var buf bytes.Buffer
	// Claim 1KiB of header, supply only 4 bytes.
	_ = binary.Write(&buf, binary.LittleEndian, uint64(1024))
	buf.WriteString("oops")
	_, err := readSafetensors(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("expected error on truncated header, got nil")
	}
}

// TestReadSafetensors_SkipsIntegerTensors verifies that integer tensors are skipped while float tensors are decoded.
func TestReadSafetensors_SkipsIntegerTensors(t *testing.T) {
	// "mapping" is I64 (skipped); "embeddings" is F32 (kept).
	header := `{"mapping":{"dtype":"I64","shape":[2],"data_offsets":[0,16]},` +
		`"embeddings":{"dtype":"F32","shape":[1,2],"data_offsets":[16,24]}}`
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(header)))
	buf.WriteString(header)
	buf.Write(make([]byte, 16)) // mapping payload
	_ = binary.Write(&buf, binary.LittleEndian, []float32{1, 2})
	out, err := readSafetensors(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := out["mapping"]; ok {
		t.Error("integer mapping tensor should have been skipped")
	}
	if _, ok := out["embeddings"]; !ok {
		t.Error("float embeddings tensor should have been decoded")
	}
}

func keysOf(m map[string]Tensor) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
