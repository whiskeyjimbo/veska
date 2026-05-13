// Package cohnsw implements the eval.VectorIndex interface using
// github.com/coder/hnsw (pure Go, no CGo, float32 only, file persistence).
//
// CGo requirement: none.
// Quantization: float32 only.
package cohnsw

import (
	"bufio"
	"fmt"
	"os"

	"github.com/coder/hnsw"
)

// Index wraps a coder/hnsw Graph to implement eval.VectorIndex.
type Index struct {
	g *hnsw.Graph[uint64]
	n int
}

// New creates a new coder/hnsw Index with M=16, EfSearch=100.
func New() *Index {
	g := hnsw.NewGraph[uint64]()
	g.M = 16
	g.EfSearch = 100
	g.Distance = hnsw.EuclideanDistance
	return &Index{g: g}
}

// Add inserts a vector with the given id.
func (c *Index) Add(id uint64, vec []float32) error {
	c.g.Add(hnsw.MakeNode(id, vec))
	c.n++
	return nil
}

// Search returns the k nearest ids for the query vector.
func (c *Index) Search(query []float32, k int) ([]uint64, error) {
	nodes := c.g.Search(query, k)
	ids := make([]uint64, len(nodes))
	for i, n := range nodes {
		ids[i] = n.Key
	}
	return ids, nil
}

// Save writes the graph to path using the built-in Export function.
func (c *Index) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("cohnsw: create file: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	if err := c.g.Export(w); err != nil {
		return fmt.Errorf("cohnsw: export: %w", err)
	}
	return w.Flush()
}

// Load reads the graph from path.
func (c *Index) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cohnsw: open file: %w", err)
	}
	defer f.Close()
	g := hnsw.NewGraph[uint64]()
	g.Distance = hnsw.EuclideanDistance
	if err := g.Import(bufio.NewReader(f)); err != nil {
		return fmt.Errorf("cohnsw: import: %w", err)
	}
	c.g = g
	c.n = g.Len()
	return nil
}

// Len returns the number of indexed vectors.
func (c *Index) Len() int {
	return c.n
}
