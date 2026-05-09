// Package vecbin provides read/write helpers for the flat binary vector file format.
//
// Format: little-endian uint64 count, then count*768 little-endian float32 values.
// There is no per-vector dimension prefix.
package vecbin

import (
	"encoding/binary"
	"fmt"
	"io"
)

const Dim = 768

// WriteVectors writes vecs to w in the flat binary format.
func WriteVectors(w io.Writer, vecs [][]float32) error {
	count := uint64(len(vecs))
	if err := binary.Write(w, binary.LittleEndian, count); err != nil {
		return fmt.Errorf("vecbin: write count: %w", err)
	}
	for i, v := range vecs {
		if len(v) != Dim {
			return fmt.Errorf("vecbin: vec[%d] has dim %d, want %d", i, len(v), Dim)
		}
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return fmt.Errorf("vecbin: write vec[%d]: %w", i, err)
		}
	}
	return nil
}

// ReadVectors reads all vectors from r in the flat binary format.
func ReadVectors(r io.Reader) ([][]float32, error) {
	var count uint64
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("vecbin: read count: %w", err)
	}
	vecs := make([][]float32, count)
	for i := range vecs {
		v := make([]float32, Dim)
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			return nil, fmt.Errorf("vecbin: read vec[%d]: %w", i, err)
		}
		vecs[i] = v
	}
	return vecs, nil
}
