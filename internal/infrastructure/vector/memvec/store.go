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
	"sort"
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

// Search returns the k nearest neighbors of the query vector in the specified repository and branch.
// It performs a brute-force L2-squared linear scan and returns results sorted by score in descending
// order. If filter.ModelID is specified, only that partition is searched; otherwise, results from all
// partitions for the repository and branch are merged.
func (s *Store) Search(_ context.Context, repoID, branch string, vec []float32, k int, filter domain.VectorFilter) ([]domain.SearchHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		nodeID string
		dist   float32
	}
	var cands []candidate

	scan := func(p map[string]*row) {
		for _, r := range p {
			d := l2sq(r.vector, vec)
			cands = append(cands, candidate{nodeID: r.nodeID, dist: d})
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

	// Sort candidates by ascending distance to find the closest matches first.
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })

	if k > len(cands) {
		k = len(cands)
	}
	cands = cands[:k]

	hits := make([]domain.SearchHit, len(cands))
	for i, c := range cands {
		hits[i] = domain.SearchHit{
			NodeID: c.nodeID,
			Score:  1.0 / (1.0 + c.dist),
		}
	}
	return hits, nil
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
	var sum float64
	for i := range n {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return float32(sum)
}
