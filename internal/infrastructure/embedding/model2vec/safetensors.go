package model2vec

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

// safetensors file layout:
//	+--------+--------------------+--------------------+
//	| u64 LE | header (UTF-8 JSON)| concatenated data |
//	| N | N bytes | rest of file |
//	+--------+--------------------+--------------------+
// Header schema (Model2Vec only uses the embedding-matrix entry):
//	{
//	  "tensor_name": {
//	    "dtype": "F32" | "F16",
//	    "shape": [int,.],
//	    "data_offsets": [start, end] // relative to start of data segment
//	  },
//	}
// The "__metadata__" key is optional and ignored here — Model2Vec
// doesn't depend on it for inference.

// Tensor is one decoded entry from a safetensors file. Data is
// flattened in row-major order; callers slice it according to Shape.
// The float values are normalised to float32 regardless of whether the
// source dtype was F16 or F32 so downstream math has one type.
type Tensor struct {
	Dtype string
	Shape []int
	Data  []float32
}

type safetensorsHeaderEntry struct {
	Dtype       string `json:"dtype"`
	Shape       []int  `json:"shape"`
	DataOffsets []int  `json:"data_offsets"`
}

// readSafetensors parses the safetensors envelope from r and returns
// one decoded Tensor per named entry. The "__metadata__" key is
// silently skipped — it carries optional model metadata, not tensors.
// Float tensors (F32/F16/F64) are decoded to float32. Tensors with an
// integer dtype (e.g. the identity I64 "mapping" tensor potion models
// ship) are skipped rather than rejected — they aren't used in the
// float pooling math, and erroring on a perfectly valid model file is
// worse than ignoring an unused tensor. A genuinely corrupt float
// tensor still surfaces via the decode error path below.
func readSafetensors(r io.Reader) (map[string]Tensor, error) {
	var hdrLen uint64
	if err := binary.Read(r, binary.LittleEndian, &hdrLen); err != nil {
		return nil, fmt.Errorf("safetensors: read header length: %w", err)
	}
	if hdrLen == 0 || hdrLen > 1<<30 { // 1 GiB sanity ceiling
		return nil, fmt.Errorf("safetensors: implausible header length %d", hdrLen)
	}
	hdrBuf := make([]byte, hdrLen)
	if _, err := io.ReadFull(r, hdrBuf); err != nil {
		return nil, fmt.Errorf("safetensors: read header (%d bytes): %w", hdrLen, err)
	}

	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(hdrBuf, &hdr); err != nil {
		return nil, fmt.Errorf("safetensors: parse header json: %w", err)
	}

	// Read the rest of the file as the data segment — sized by the
	// largest data_offsets[end] across all tensors.
	type entry struct {
		name string
		safetensorsHeaderEntry
	}
	entries := make([]entry, 0, len(hdr))
	var dataEnd int
	for name, raw := range hdr {
		if name == "__metadata__" {
			continue
		}
		var e safetensorsHeaderEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("safetensors: parse entry %q: %w", name, err)
		}
		if len(e.DataOffsets) != 2 || e.DataOffsets[0] < 0 || e.DataOffsets[1] < e.DataOffsets[0] {
			return nil, fmt.Errorf("safetensors: entry %q: bad data_offsets %v", name, e.DataOffsets)
		}
		entries = append(entries, entry{name: name, safetensorsHeaderEntry: e})
		if e.DataOffsets[1] > dataEnd {
			dataEnd = e.DataOffsets[1]
		}
	}
	dataBuf := make([]byte, dataEnd)
	if _, err := io.ReadFull(r, dataBuf); err != nil {
		return nil, fmt.Errorf("safetensors: read tensor data (%d bytes): %w", dataEnd, err)
	}

	out := make(map[string]Tensor, len(entries))
	for _, e := range entries {
		if !isFloatDtype(e.Dtype) {
			continue // skip integer/aux tensors (e.g. identity "mapping")
		}
		raw := dataBuf[e.DataOffsets[0]:e.DataOffsets[1]]
		floats, err := decodeTensorBytes(e.Dtype, raw)
		if err != nil {
			return nil, fmt.Errorf("safetensors: entry %q: %w", e.name, err)
		}
		out[e.name] = Tensor{
			Dtype: e.Dtype,
			Shape: e.Shape,
			Data:  floats,
		}
	}
	return out, nil
}

// isFloatDtype reports whether dtype is one decodeTensorBytes can turn
// into float32. Non-float tensors are skipped at load time.
func isFloatDtype(dtype string) bool {
	switch dtype {
	case "F16", "F32", "F64":
		return true
	default:
		return false
	}
}

// decodeTensorBytes interprets raw bytes as a flat float32 slice. F16
// and F64 values are converted to float32 (the cost is one widen/narrow
// per element, happens once at model load). potion-* models store the
// per-token "weights" tensor as F64.
func decodeTensorBytes(dtype string, raw []byte) ([]float32, error) {
	switch dtype {
	case "F32":
		if len(raw)%4 != 0 {
			return nil, fmt.Errorf("F32 byte length %d not divisible by 4", len(raw))
		}
		out := make([]float32, len(raw)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		return out, nil
	case "F64":
		if len(raw)%8 != 0 {
			return nil, fmt.Errorf("F64 byte length %d not divisible by 8", len(raw))
		}
		out := make([]float32, len(raw)/8)
		for i := range out {
			out[i] = float32(math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:])))
		}
		return out, nil
	case "F16":
		if len(raw)%2 != 0 {
			return nil, fmt.Errorf("F16 byte length %d not divisible by 2", len(raw))
		}
		out := make([]float32, len(raw)/2)
		for i := range out {
			bits := binary.LittleEndian.Uint16(raw[i*2:])
			out[i] = float16ToFloat32(bits)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported dtype %q (want F32 or F16)", dtype)
	}
}

// float16ToFloat32 converts an IEEE 754 half-precision float to single
// precision. Handles sub-normals, infinities, and NaN by the standard
// bit-juggling recipe — fast and allocation-free.
func float16ToFloat32(h uint16) float32 {
	sign := uint32(h>>15) & 0x1
	exp := uint32(h>>10) & 0x1F
	frac := uint32(h) & 0x3FF
	var bits uint32
	switch {
	case exp == 0 && frac == 0:
		bits = sign << 31
	case exp == 0:
		// Subnormal: normalise into single-precision form.
		e := uint32(1)
		for frac&0x400 == 0 {
			frac <<= 1
			e++
		}
		frac &= 0x3FF
		bits = (sign << 31) | ((127 - 15 - e + 1) << 23) | (frac << 13)
	case exp == 0x1F:
		// Inf / NaN.
		bits = (sign << 31) | (0xFF << 23) | (frac << 13)
	default:
		bits = (sign << 31) | ((exp + (127 - 15)) << 23) | (frac << 13)
	}
	return math.Float32frombits(bits)
}
