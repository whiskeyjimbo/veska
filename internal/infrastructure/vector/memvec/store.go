// SPDX-License-Identifier: AGPL-3.0-only

// Package memvec provides a VectorStorage implementation backed by an in-memory
// map with linear-scan nearest-neighbor search.
//
// This backend serves as the zero-extra-native-dependency default for veska, requiring
// no external shared libraries. It is adequate for repositories with fewer than YellowThreshold
// (~75k) embedded nodes. The search implementation performs a brute-force L2-squared
// linear scan over all stored vectors. For larger workspaces, configuring the usearch backend
// is recommended.
package memvec

import (
	"context"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// YellowThreshold is the vector count above which veska doctor storage
// emits a yellow (warning) ceiling alert for this backend.
const YellowThreshold = 75_000

// RedThreshold is the vector count above which veska doctor storage
// emits a red (error) ceiling alert for this backend.
const RedThreshold = 90_000

// storeKey uniquely identifies a partition mapped by repository, branch, and model identifier.
type storeKey struct {
	repoID  string
	branch  string
	modelID string
}

// row holds a stored embedding and its associated metadata.
type row struct {
	nodeID      string
	vector      []float32
	contentHash string
}

// Store implements an in-memory VectorStorage that performs linear scans for nearest
// neighbor queries. It is safe for concurrent use. Because search complexity is O(n·d)
// per query, the usearch backend should be configured for workspaces exceeding YellowThreshold.
type Store struct {
	mu         sync.RWMutex
	partitions map[storeKey]map[string]*row // key → nodeID → row
}

// Compile-time check to ensure Store implements the ports.VectorStorage interface.
var _ ports.VectorStorage = (*Store)(nil)

// New constructs and returns a new empty Store instance.
func New() *Store {
	return &Store{
		partitions: make(map[storeKey]map[string]*row),
	}
}

// partition returns the nodeID-to-row map for the given partition key, creating it if it is missing.
// The caller must hold the write lock on the store mutex.
func (s *Store) partition(repoID, branch, modelID string) map[string]*row {
	k := storeKey{repoID: repoID, branch: branch, modelID: modelID}
	if p, ok := s.partitions[k]; ok {
		return p
	}
	p := make(map[string]*row)
	s.partitions[k] = p
	return p
}

// UpsertEmbeddings inserts or replaces rows for the specified repository and branch.
// Each row's ModelID determines the partition it belongs to.
func (s *Store) UpsertEmbeddings(_ context.Context, repoID, branch string, batch []domain.EmbeddingRow) error {
	if len(batch) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, b := range batch {
		p := s.partition(repoID, branch, b.ModelID)
		vec := make([]float32, len(b.Vector))
		copy(vec, b.Vector)
		p[b.NodeID] = &row{
			nodeID:      b.NodeID,
			vector:      vec,
			contentHash: b.ContentHash,
		}
	}
	return nil
}

// candidate is a node and its squared L2 distance to the query vector.
type candidate struct {
	nodeID string
	dist   float32
}

// Search returns the k nearest neighbors of the query vector in the specified repository and branch.
// It performs a brute-force L2-squared linear scan and returns results sorted by score in descending
// order. If filter.ModelID is specified, only that partition is searched; otherwise, results from all
// partitions for the repository and branch are merged.
//
// It keeps only the k closest candidates in a bounded max-heap (root = the
// farthest of the kept k) rather than collecting every vector and sorting the
// whole set: allocation is O(k) per query instead of O(N), and there is no
// full-partition sort. l2sq is still evaluated once per stored vector - the scan
// itself is inherently linear for this backend.
func (s *Store) Search(_ context.Context, repoID, branch string, vec []float32, k int, filter domain.VectorFilter) ([]domain.SearchHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if k <= 0 {
		return nil, nil
	}

	// Max-heap of the k closest candidates seen so far. A new candidate enters
	// only when it is closer than the current farthest kept (the root).
	h := make([]candidate, 0, k)
	consider := func(nodeID string, d float32) {
		if len(h) < k {
			h = append(h, candidate{nodeID: nodeID, dist: d})
			siftUp(h, len(h)-1)
			return
		}
		if d < h[0].dist {
			h[0] = candidate{nodeID: nodeID, dist: d}
			siftDown(h, 0)
		}
	}

	scan := func(p map[string]*row) {
		for _, r := range p {
			consider(r.nodeID, l2sq(r.vector, vec))
		}
	}

	if filter.ModelID != "" {
		k2 := storeKey{repoID: repoID, branch: branch, modelID: filter.ModelID}
		if p, ok := s.partitions[k2]; ok {
			scan(p)
		}
	} else {
		for k2, p := range s.partitions {
			if k2.repoID == repoID && k2.branch == branch {
				scan(p)
			}
		}
	}

	// Drain the heap farthest-first, filling the result back-to-front so it ends
	// up sorted by ascending distance (descending score).
	hits := make([]domain.SearchHit, len(h))
	for i := len(h) - 1; i >= 0; i-- {
		top := h[0]
		last := len(h) - 1
		h[0] = h[last]
		h = h[:last]
		if len(h) > 0 {
			siftDown(h, 0)
		}
		hits[i] = domain.SearchHit{
			NodeID: top.nodeID,
			Score:  1.0 / (1.0 + top.dist),
		}
	}
	return hits, nil
}

// siftUp restores the max-heap property after appending at index i.
func siftUp(h []candidate, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h[parent].dist >= h[i].dist {
			break
		}
		h[parent], h[i] = h[i], h[parent]
		i = parent
	}
}

// siftDown restores the max-heap property after replacing the element at index i.
func siftDown(h []candidate, i int) {
	n := len(h)
	for {
		largest := i
		if l := 2*i + 1; l < n && h[l].dist > h[largest].dist {
			largest = l
		}
		if r := 2*i + 2; r < n && h[r].dist > h[largest].dist {
			largest = r
		}
		if largest == i {
			break
		}
		h[i], h[largest] = h[largest], h[i]
		i = largest
	}
}

// Reindex is a no-op because the linear-scan store does not maintain a persistent index structure.
func (s *Store) Reindex(_ context.Context, _ string, _ string) error {
	return nil
}

// LookupContentHashes returns a map of node IDs to their content hashes for the specified
// repository and branch. Identifiers that do not exist are silently omitted.
func (s *Store) LookupContentHashes(_ context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	want := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		want[id] = struct{}{}
	}

	result := make(map[string]string, len(nodeIDs))
	for k2, p := range s.partitions {
		if k2.repoID != repoID || k2.branch != branch {
			continue
		}
		for nodeID, r := range p {
			if _, ok := want[nodeID]; ok {
				result[nodeID] = r.contentHash
			}
		}
	}
	return result, nil
}

// VectorCount returns the total number of stored vectors across all partitions
// for the given repository and branch. This is utilized by the doctor storage checks.
func (s *Store) VectorCount(repoID, branch string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0
	for k2, p := range s.partitions {
		if k2.repoID == repoID && k2.branch == branch {
			total += len(p)
		}
	}
	return total
}

// TotalVectorCount returns the total number of stored vectors across all partitions in the store.
func (s *Store) TotalVectorCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0
	for _, p := range s.partitions {
		total += len(p)
	}
	return total
}

// l2sq calculates the squared Euclidean distance between two vectors. It does not
// compute the square root, matching the usearch L2sq metric behavior. If the vectors have
// differing lengths, comparison is bounded by the length of the shorter vector.
func l2sq(a, b []float32) float32 {
	n := min(len(b), len(a))
	// Reslice to n so the compiler can eliminate per-iteration bounds checks on
	// the unrolled indices.
	a = a[:n]
	b = b[:n]

	// Four independent accumulators break the loop-carried dependency on a single
	// sum, letting the CPU run the multiply-adds in parallel. Accumulating in
	// float32 (not float64) also drops two per-element conversions; precision is
	// sufficient for ranking L2-normalized embeddings, and the pairwise final add
	// keeps the rounding error lower than a single running float32 sum.
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= n; i += 4 {
		d0 := a[i] - b[i]
		d1 := a[i+1] - b[i+1]
		d2 := a[i+2] - b[i+2]
		d3 := a[i+3] - b[i+3]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
	}
	sum := (s0 + s2) + (s1 + s3)
	for ; i < n; i++ {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}
