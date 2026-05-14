// Package sqlitevec provides a VectorStorage implementation backed by an
// in-memory map with linear-scan nearest-neighbour search.
//
// # Design note
//
// This backend is the zero-extra-native-dependency default for veska. It requires
// no external shared libraries (no libusearch_c.so, no liblancedb_go.a) and is
// adequate for repositories with fewer than SQLiteVecYellowThreshold (~75k) embedded
// nodes. Above that threshold veska doctor storage will emit a warning, and above
// SQLiteVecRedThreshold (~90k) an error-level warning is shown.
//
// The search implementation is a brute-force L2-squared linear scan over all stored
// vectors. This is intentional: for small corpora the scan is fast enough (sub-ms
// for <75k 768-dim vectors on modern hardware), and the simplicity eliminates any
// dependency on HNSW native libraries during development, CI, and small-scale
// production use.
//
// For workspaces that exceed 75k nodes, switch to the usearch backend via the
// vector.backend configuration key (see internal/infrastructure/vector/backend.go).
package sqlitevec

import (
	"context"
	"sort"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// SQLiteVecYellowThreshold is the vector count above which veska doctor storage
// emits a yellow (warning) ceiling alert for this backend.
const SQLiteVecYellowThreshold = 75_000

// SQLiteVecRedThreshold is the vector count above which veska doctor storage
// emits a red (error) ceiling alert for this backend.
const SQLiteVecRedThreshold = 90_000

// storeKey uniquely identifies a per-(repoID, branch, modelID) partition.
type storeKey struct {
	repoID  string
	branch  string
	modelID string
}

// row holds one stored embedding and its metadata.
type row struct {
	nodeID      string
	vector      []float32
	contentHash string
}

// SQLiteVecStore is an in-memory VectorStorage that uses linear scan for ANN
// queries. It is safe for concurrent use.
//
// WARNING: This is a dev/low-count backend. Search is O(n·d) per query.
// Use the usearch backend for workspaces exceeding SQLiteVecYellowThreshold nodes.
type SQLiteVecStore struct {
	mu         sync.RWMutex
	partitions map[storeKey]map[string]*row // key → nodeID → row
}

// Compile-time interface check.
var _ ports.VectorStorage = (*SQLiteVecStore)(nil)

// New returns an empty SQLiteVecStore.
func New() *SQLiteVecStore {
	return &SQLiteVecStore{
		partitions: make(map[storeKey]map[string]*row),
	}
}

// partition returns (creating if needed) the nodeID→row map for the given key.
// Caller must hold the write lock.
func (s *SQLiteVecStore) partition(repoID, branch, modelID string) map[string]*row {
	k := storeKey{repoID: repoID, branch: branch, modelID: modelID}
	if p, ok := s.partitions[k]; ok {
		return p
	}
	p := make(map[string]*row)
	s.partitions[k] = p
	return p
}

// UpsertEmbeddings inserts or replaces rows for (repoID, branch). Each row's
// ModelID determines its partition.
func (s *SQLiteVecStore) UpsertEmbeddings(_ context.Context, repoID, branch string, batch []domain.EmbeddingRow) error {
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

// Search returns the k nearest neighbours of vec in (repoID, branch) using
// brute-force L2-squared linear scan. Results are sorted by score descending
// (score = 1/(1+dist)).
//
// If filter.ModelID is non-empty, only the matching partition is searched;
// otherwise all model partitions for the (repoID, branch) pair are merged.
func (s *SQLiteVecStore) Search(_ context.Context, repoID, branch string, vec []float32, k int, filter domain.Filter) ([]domain.Hit, error) {
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

	// Sort by ascending distance (closest first).
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })

	if k > len(cands) {
		k = len(cands)
	}
	cands = cands[:k]

	hits := make([]domain.Hit, len(cands))
	for i, c := range cands {
		hits[i] = domain.Hit{
			NodeID: c.nodeID,
			Score:  1.0 / (1.0 + c.dist),
		}
	}
	return hits, nil
}

// Reindex is a no-op. The linear-scan store has no persistent index structure
// to rebuild.
func (s *SQLiteVecStore) Reindex(_ context.Context, _ string, _ string) error {
	return nil
}

// LookupContentHashes returns a nodeID → contentHash map for the given node IDs
// within (repoID, branch). Missing IDs are silently omitted.
func (s *SQLiteVecStore) LookupContentHashes(_ context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error) {
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
// for the given (repoID, branch). Used by the doctor storage backend check.
func (s *SQLiteVecStore) VectorCount(repoID, branch string) int {
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

// TotalVectorCount returns the total number of stored vectors across all
// partitions. Used by the doctor storage backend check.
func (s *SQLiteVecStore) TotalVectorCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0
	for _, p := range s.partitions {
		total += len(p)
	}
	return total
}

// l2sq returns the squared Euclidean distance between a and b (sum of squared
// differences — no square root). Vectors of different lengths are compared up
// to the shorter one. This matches the usearch L2sq metric and is cheaper than
// L2 distance for ranking purposes (monotonic, no Sqrt needed).
func l2sq(a, b []float32) float32 {
	n := min(len(b), len(a))
	var sum float64
	for i := range n {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return float32(sum)
}
