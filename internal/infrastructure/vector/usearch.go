// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector

import (
	"context"
	"fmt"
	"sort"
	"sync"

	usearchlib "github.com/unum-cloud/usearch/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	indexDim             = 768
	indexConnectivity    = 16
	indexExpansionAdd    = 64 // An expansion factor of 64 is the practical default because higher values are slower with negligible recall gain.
	indexExpansionSearch = 100
)

// indexKey uniquely identifies an HNSW index partitioned by repository, branch, and model.
type indexKey struct {
	repoID  string
	branch  string
	modelID string
}

// rowMeta stores the Go-side metadata associated with each usearch slot. The original
// float32 vector is omitted because usearch maintains its own float16-quantized copy;
// keeping a duplicate copy in Go memory would double the per-vector RSS without benefit.
type rowMeta struct {
	NodeID      string
	ContentHash string
	ModelID     string
}

// indexEntry manages a single native usearch index alongside the auxiliary maps
// required to resolve content hashes and map numeric usearch identifiers back to Go node IDs.
type indexEntry struct {
	idx      *usearchlib.Index
	rows     map[uint64]rowMeta // Maps numeric usearch identifiers to row metadata.
	nodeToID map[string]uint64  // Maps node identifiers to usearch identifiers.
	nextID   uint64
	capacity uint
}

func newIndexEntry() (*indexEntry, error) {
	conf := usearchlib.IndexConfig{
		Dimensions:      indexDim,
		Metric:          usearchlib.L2sq,
		Quantization:    usearchlib.F16,
		Connectivity:    indexConnectivity,
		ExpansionAdd:    indexExpansionAdd,
		ExpansionSearch: indexExpansionSearch,
	}
	idx, err := usearchlib.NewIndex(conf)
	if err != nil {
		return nil, fmt.Errorf("usearch: new index: %w", err)
	}
	return &indexEntry{
		idx:      idx,
		rows:     make(map[uint64]rowMeta),
		nodeToID: make(map[string]uint64),
	}, nil
}

// reserve pre-allocates space in the native index to accommodate at least n additional vectors.
func (e *indexEntry) reserve(needed uint) error {
	total := uint(len(e.rows)) + needed
	if total <= e.capacity {
		return nil
	}
	newCap := total*2 + 1024
	if err := e.idx.Reserve(newCap); err != nil {
		return fmt.Errorf("usearch: reserve %d: %w", newCap, err)
	}
	e.capacity = newCap
	return nil
}

// upsert inserts or replaces a single row in the index. Because the underlying usearch
// index does not support in-place updates, any pre-existing entry is untracked in the
// metadata maps and a brand new identifier is generated for the updated vector.
func (e *indexEntry) upsert(row domain.EmbeddingRow) error {
	if oldID, exists := e.nodeToID[row.NodeID]; exists {
		// Remove the old metadata. The obsolete slot in the usearch index is left
		// as a tombstone because the usearch API does not support deletion.
		delete(e.rows, oldID)
	}
	if err := e.reserve(1); err != nil {
		return err
	}
	id := e.nextID
	e.nextID++
	if err := e.idx.Add(id, row.Vector); err != nil {
		return fmt.Errorf("usearch: add id=%d nodeID=%q: %w", id, row.NodeID, err)
	}
	e.rows[id] = rowMeta{NodeID: row.NodeID, ContentHash: row.ContentHash, ModelID: row.ModelID}
	e.nodeToID[row.NodeID] = id
	return nil
}

// UsearchStore implements the VectorStorage interface using separate, in-memory usearch
// HNSW indexes partitioned by repository, branch, and model. It uses float16 quantization
// and is safe for concurrent access.
type UsearchStore struct {
	mu      sync.RWMutex
	indexes map[indexKey]*indexEntry
}

// Compile-time check to ensure UsearchStore implements the ports.VectorStorage interface.
var _ ports.VectorStorage = (*UsearchStore)(nil)

// NewUsearchStore constructs an empty UsearchStore instance.
func NewUsearchStore() (*UsearchStore, error) {
	return &UsearchStore{
		indexes: make(map[indexKey]*indexEntry),
	}, nil
}

// Destroy releases all native usearch resources. This method must be called when
// the store is no longer needed to prevent memory leaks in the C runtime.
func (s *UsearchStore) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.indexes {
		_ = e.idx.Destroy()
		delete(s.indexes, k)
	}
}

// getOrCreate returns the indexEntry for the given key, constructing it if it does not exist.
// The caller must hold the write lock on the store mutex.
func (s *UsearchStore) getOrCreate(key indexKey) (*indexEntry, error) {
	if e, ok := s.indexes[key]; ok {
		return e, nil
	}
	e, err := newIndexEntry()
	if err != nil {
		return nil, err
	}
	s.indexes[key] = e
	return e, nil
}

// UpsertEmbeddings inserts or updates a batch of embedding rows partitioned by repository and branch.
// Each row is stored in the specific HNSW index corresponding to its model identifier.
// The index capacity is reserved in advance to prevent repeated reallocation overhead.
func (s *UsearchStore) UpsertEmbeddings(_ context.Context, repoID, branch string, batch []domain.EmbeddingRow) error {
	if len(batch) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Count the rows required per model to perform a single pre-allocation for each index.
	perModel := make(map[string]uint, 4)
	for _, row := range batch {
		perModel[row.ModelID] += 1
	}
	for modelID, n := range perModel {
		key := indexKey{repoID: repoID, branch: branch, modelID: modelID}
		e, err := s.getOrCreate(key)
		if err != nil {
			return err
		}
		if err := e.reserve(n); err != nil {
			return err
		}
	}

	for _, row := range batch {
		key := indexKey{repoID: repoID, branch: branch, modelID: row.ModelID}
		if err := s.indexes[key].upsert(row); err != nil {
			return err
		}
	}
	return nil
}

// Search returns the k nearest neighbors to the query vector within the specified repository and branch.
// If filter.ModelID is specified, only that model's index is queried; otherwise, searches are executed
// across all active model indexes for the repository and branch and the results are merged.
// The results are returned sorted by score in descending order, where the similarity score is derived
// from the L2-squared distance.
func (s *UsearchStore) Search(_ context.Context, repoID, branch string, vec []float32, k int, filter domain.VectorFilter) ([]domain.SearchHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type candidate struct {
		nodeID string
		dist   float32
	}
	var candidates []candidate

	search := func(e *indexEntry) error {
		if e == nil || len(e.rows) == 0 {
			return nil
		}
		want := uint(k)
		if uint(len(e.rows)) < want {
			want = uint(len(e.rows))
		}
		keys, dists, err := e.idx.Search(vec, want)
		if err != nil {
			return fmt.Errorf("usearch: search: %w", err)
		}
		for i, key := range keys {
			row, ok := e.rows[key]
			if !ok {
				// Skip slots that have been overwritten and left as tombstones.
				continue
			}
			var dist float32
			if i < len(dists) {
				dist = dists[i]
			}
			candidates = append(candidates, candidate{nodeID: row.NodeID, dist: dist})
		}
		return nil
	}

	if filter.ModelID != "" {
		key := indexKey{repoID: repoID, branch: branch, modelID: filter.ModelID}
		e := s.indexes[key] // This will be nil if the index has not been created.
		if err := search(e); err != nil {
			return nil, err
		}
	} else {
		for key, e := range s.indexes {
			if key.repoID != repoID || key.branch != branch {
				continue
			}
			if err := search(e); err != nil {
				return nil, err
			}
		}
	}

	// Sort the aggregated candidates by distance in ascending order before truncating and converting to scores.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].dist < candidates[j].dist
	})
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	hits := make([]domain.SearchHit, len(candidates))
	for i, c := range candidates {
		// Convert the L2-squared distance to a similarity score between 0 and 1.
		hits[i] = domain.SearchHit{
			NodeID: c.nodeID,
			Score:  1.0 / (1.0 + c.dist),
		}
	}
	return hits, nil
}

// Reindex is a no-op implementation because the store maintains vector indexes in-memory.
// Re-quantization is natively handled at load time via the index configuration.
func (s *UsearchStore) Reindex(_ context.Context, _ string, _ string) error {
	return nil
}

// LookupContentHashes retrieves the content hashes for the specified node IDs within a repository and branch.
// Identifiers that are not present in the index are silently omitted from the returned map.
func (s *UsearchStore) LookupContentHashes(_ context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build a lookup set to check requested node IDs efficiently.
	want := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		want[id] = struct{}{}
	}

	result := make(map[string]string, len(nodeIDs))
	for key, e := range s.indexes {
		if key.repoID != repoID || key.branch != branch {
			continue
		}
		for _, row := range e.rows {
			if _, ok := want[row.NodeID]; ok {
				result[row.NodeID] = row.ContentHash
			}
		}
	}
	return result, nil
}
