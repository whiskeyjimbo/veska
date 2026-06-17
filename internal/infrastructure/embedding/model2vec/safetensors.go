package model2vec

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

// Safetensors format consists of a little-endian uint64 length header, a UTF-8 JSON header describing tensor shapes and offsets, and concatenated raw data bytes.

// Tensor represents a single decoded tensor from a Safetensors payload, with values normalized to float32.
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

// readSafetensors parses the Safetensors envelope and returns a map of decoded float tensors.
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

	// Read the rest of the file as the data segment sized by the largest data offset.
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
			continue // Skip integer and auxiliary tensors.
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

// isFloatDtype returns true if the data type is a supported float format (F16, F32, or F64).
func isFloatDtype(dtype string) bool {
	switch dtype {
	case "F16", "F32", "F64":
		return true
	default:
		return false
	}
}

// decodeTensorBytes converts raw bytes of supported formats to a float32 slice.
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

// float16ToFloat32 converts a half-precision float (F16) to single-precision float32.
func float16ToFloat32(h uint16) float32 {
	sign := uint32(h>>15) & 0x1
	exp := uint32(h>>10) & 0x1F
	frac := uint32(h) & 0x3FF
	var bits uint32
	switch {
	case exp == 0 && frac == 0:
		bits = sign << 31
	case exp == 0:
		// Normalize subnormal half-precision numbers.
		e := uint32(1)
		for frac&0x400 == 0 {
			frac <<= 1
			e++
		}
		frac &= 0x3FF
		bits = (sign << 31) | ((127 - 15 - e + 1) << 23) | (frac << 13)
	case exp == 0x1F:
		// Handle infinities and NaN cases.
		bits = (sign << 31) | (0xFF << 23) | (frac << 13)
	default:
		bits = (sign << 31) | ((exp + (127 - 15)) << 23) | (frac << 13)
	}
	return math.Float32frombits(bits)
}
