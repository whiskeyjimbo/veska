package model2vec

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// buildSafetensorsFile builds a minimal safetensors blob in memory for
// tests: a single tensor of float32 data with given name + shape.
func buildSafetensorsFile(t *testing.T, name string, shape []int, data []float32) []byte {
	t.Helper()
	// safetensors data is a flat byte buffer of tensors, declared in
	// header order with byte-offset ranges into the data segment.
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

// TestReadSafetensors_RoundTripsSingleF32: build a 2x3 F32 tensor in
// memory, write the safetensors envelope, parse it back, and assert
// the values come out identical. Pins both the header layout (u64
// little-endian length + JSON) and the F32 little-endian tensor decode.
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

// TestReadSafetensors_TruncatedHeaderErrors: a header shorter than
// declared must not silently produce empty results — that would mask
// a corrupt model file as "no embeddings", which manifests as 100%
// retrieval miss in production with no useful error.
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

// TestReadSafetensors_SkipsIntegerTensors: real potion-* models ship an
// identity I64 "mapping" tensor alongside the float matrix. We decode
// only float dtypes and skip integer/aux tensors rather than erroring —
// erroring on a valid model file is worse than ignoring an unused
// tensor. The float entry alongside it must still decode.
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
