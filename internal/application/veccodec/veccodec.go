// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package veccodec holds the little-endian float32 vector wire codec shared by
// the embedder, autolink, and MCP similarity consumers. It is a zero-dependency
// leaf: the decode routine was hand-copied into three packages (embedder,
// autolink, infrastructure/mcp) to avoid upward sibling imports; once a third
// copy appeared the duplication was promoted here rather than copied again
package veccodec

import (
	"encoding/binary"
	"math"
)

// EncodeFloat32LE packs vec into a little-endian float32 byte blob, the wire
// format stored in node_embeddings.embedding. DecodeFloat32LE reverses it.
func EncodeFloat32LE(vec []float32) []byte {
	out := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// DecodeFloat32LE reverses a little-endian float32 blob into a slice of dim
// elements. dim is the expected element count; if the blob is short, the
// returned slice is truncated rather than panicking, so a malformed row
// degrades to "skip this hit" at the call site.
func DecodeFloat32LE(blob []byte, dim int) []float32 {
	have := len(blob) / 4
	if have < dim {
		dim = have
	}
	out := make([]float32, dim)
	for i := range dim {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4 : i*4+4]))
	}
	return out
}
