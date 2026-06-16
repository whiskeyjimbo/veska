//go:build hnsw_native

// Package usearch implements the eval.VectorIndex interface using
// github.com/unum-cloud/usearch/golang (CGo, C++17, float32/float16/int8 quantization).
// CGo requirement: yes — requires libusearch_c.so and usearch.h at build time.
// Quantization: float32, float16, int8 (recorded in eval run).
// Build with: go build/test -tags hnsw_native
package usearch

import (
	"fmt"

	usearchlib "github.com/unum-cloud/usearch/golang"
)

const (
	dim             = 768
	connectivity    = 16
	expansionAdd    = 200
	expansionSearch = 100
)

// Index wraps a usearch HNSW index to implement eval.VectorIndex.
type Index struct {
	idx      *usearchlib.Index
	conf     usearchlib.IndexConfig
	quant    usearchlib.Quantization
	n        int
	capacity uint
}

// New creates a new usearch Index with the given quantization level.
// quant should be usearchlib.Float32, usearchlib.Float16, or usearchlib.Int8.
func New(quant usearchlib.Quantization) (*Index, error) {
	conf := usearchlib.IndexConfig{
		Dimensions:      dim,
		Metric:          usearchlib.L2sq,
		Quantization:    quant,
		Connectivity:    connectivity,
		ExpansionAdd:    expansionAdd,
		ExpansionSearch: expansionSearch,
	}
	idx, err := usearchlib.NewIndex(conf)
	if err != nil {
		return nil, fmt.Errorf("usearch: new index: %w", err)
	}
	return &Index{idx: idx, conf: conf, quant: quant}, nil
}

// Reserve pre-allocates space for n vectors. Must be called before Add.
func (u *Index) Reserve(n uint) error {
	if err := u.idx.Reserve(n); err != nil {
		return fmt.Errorf("usearch: reserve %d: %w", n, err)
	}
	u.capacity = n
	return nil
}

// Add inserts a vector with the given id.
// Automatically doubles capacity if the index is full.
func (u *Index) Add(id uint64, vec []float32) error {
	if uint(u.n) >= u.capacity {
		newCap := u.capacity*2 + 1024
		if err := u.idx.Reserve(newCap); err != nil {
			return fmt.Errorf("usearch: auto-reserve %d: %w", newCap, err)
		}
		u.capacity = newCap
	}
	if err := u.idx.Add(id, vec); err != nil {
		return fmt.Errorf("usearch: add id=%d: %w", id, err)
	}
	u.n++
	return nil
}

// Search returns the k nearest ids for the query vector.
func (u *Index) Search(query []float32, k int) ([]uint64, error) {
	keys, _, err := u.idx.Search(query, uint(k))
	if err != nil {
		return nil, fmt.Errorf("usearch: search: %w", err)
	}
	return keys, nil
}

// Save persists the index to path.
func (u *Index) Save(path string) error {
	if err := u.idx.Save(path); err != nil {
		return fmt.Errorf("usearch: save: %w", err)
	}
	return nil
}

// Load restores the index from path (creates a new index and loads into it).
func (u *Index) Load(path string) error {
	newIdx, err := usearchlib.NewIndex(u.conf)
	if err != nil {
		return fmt.Errorf("usearch: new index for load: %w", err)
	}
	if err := newIdx.Load(path); err != nil {
		_ = newIdx.Destroy()
		return fmt.Errorf("usearch: load: %w", err)
	}
	if err := u.idx.Destroy(); err != nil {
		return fmt.Errorf("usearch: destroy old index: %w", err)
	}
	u.idx = newIdx
	n, err := newIdx.Len()
	if err != nil {
		return fmt.Errorf("usearch: len after load: %w", err)
	}
	u.n = int(n)
	return nil
}

// Len returns the number of indexed vectors.
func (u *Index) Len() int {
	return u.n
}

// Destroy releases native resources. Must be called when done.
func (u *Index) Destroy() error {
	return u.idx.Destroy()
}
