// Package veccodec holds the little-endian float32 vector wire codec shared by
// the embedder, autolink, and MCP similarity consumers. It is a zero-dependency
// leaf: the decode routine was hand-copied into three packages (embedder,
// autolink, infrastructure/mcp) to avoid upward sibling imports; once a third
// copy appeared the duplication was promoted here rather than copied again
// (solov2-xde2.21).
package veccodec

import (
	"encoding/binary"
	"math"
)

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
