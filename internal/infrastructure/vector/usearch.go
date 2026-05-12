//go:build hnsw_native

package vector

import (
	"context"
	"fmt"
	"sort"
	"sync"

	usearchlib "github.com/unum-cloud/usearch/golang"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
	"github.com/whiskeyjimbo/engram/solov2/internal/core/ports"
)

const (
	indexDim             = 768
	indexConnectivity    = 16
	indexExpansionAdd    = 200
	indexExpansionSearch = 100
)

// indexKey uniquely identifies a per-(repoID, branch, modelID) HNSW index.
type indexKey struct {
	repoID  string
	branch  string
	modelID string
}

// indexEntry holds one usearch HNSW index plus the metadata needed to implement
// LookupContentHashes and to map usearch uint64 IDs back to nodeIDs.
type indexEntry struct {
	idx      *usearchlib.Index
	rows     map[uint64]domain.EmbeddingRow // usearch id → row
	nodeToID map[string]uint64              // nodeID → current usearch id
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
		rows:     make(map[uint64]domain.EmbeddingRow),
		nodeToID: make(map[string]uint64),
	}, nil
}

// reserve ensures the index has capacity for at least n additional vectors.
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

// upsert inserts or replaces a single row in the index.
// usearch does not support in-place updates, so an existing entry is removed from
// the metadata maps and a fresh ID is assigned.
func (e *indexEntry) upsert(row domain.EmbeddingRow) error {
	if oldID, exists := e.nodeToID[row.NodeID]; exists {
		// Remove old metadata; the old slot in usearch is left as a tombstone
		// (usearch has no Delete API in this version).
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
	e.rows[id] = row
	e.nodeToID[row.NodeID] = id
	return nil
}

// UsearchStore is an in-memory VectorStorage backed by per-(repoID,branch,modelID)
// usearch HNSW indexes with float16 quantization.
//
// It is safe for concurrent use.
type UsearchStore struct {
	mu      sync.RWMutex
	indexes map[indexKey]*indexEntry
}

// Compile-time interface satisfaction check.
var _ ports.VectorStorage = (*UsearchStore)(nil)

// NewUsearchStore constructs an empty UsearchStore.
func NewUsearchStore() (*UsearchStore, error) {
	return &UsearchStore{
		indexes: make(map[indexKey]*indexEntry),
	}, nil
}

// Destroy releases all native usearch resources. Must be called when the store is
// no longer needed to avoid leaking CGo memory.
func (s *UsearchStore) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.indexes {
		_ = e.idx.Destroy()
		delete(s.indexes, k)
	}
}

// getOrCreate returns the indexEntry for key, creating it if absent.
// Caller must hold s.mu (write lock).
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

// UpsertEmbeddings inserts or updates a batch of embedding rows under (repoID, branch).
// Each row's ModelID determines which HNSW index it is stored in.
func (s *UsearchStore) UpsertEmbeddings(_ context.Context, repoID, branch string, batch []domain.EmbeddingRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range batch {
		key := indexKey{repoID: repoID, branch: branch, modelID: row.ModelID}
		e, err := s.getOrCreate(key)
		if err != nil {
			return err
		}
		if err := e.upsert(row); err != nil {
			return err
		}
	}
	return nil
}

// Search returns the k nearest neighbours to vec within (repoID, branch).
// If filter.ModelID is non-empty, only the index for that model is searched;
// otherwise all model indexes for the (repoID, branch) pair are searched and
// results are merged.
// Results are sorted by score descending (lower L2 distance → higher score:
// score = 1 / (1 + distance)).
func (s *UsearchStore) Search(_ context.Context, repoID, branch string, vec []float32, k int, filter domain.Filter) ([]domain.Hit, error) {
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
				// tombstone slot — skip
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
		e := s.indexes[key] // nil if not found
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

	// Sort by distance ascending (closest first), then convert to score.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].dist < candidates[j].dist
	})
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	hits := make([]domain.Hit, len(candidates))
	for i, c := range candidates {
		// Convert L2-sq distance to a [0,1) similarity score.
		hits[i] = domain.Hit{
			NodeID: c.nodeID,
			Score:  1.0 / (1.0 + c.dist),
		}
	}
	return hits, nil
}

// Reindex is a no-op stub. The usearch store holds vectors in-memory; re-quantization
// is handled at load time by the index configuration.
func (s *UsearchStore) Reindex(_ context.Context, _ string, _ string) error {
	return nil
}

// LookupContentHashes returns a nodeID → contentHash map for the requested node IDs
// within (repoID, branch). Missing node IDs are silently omitted from the result.
func (s *UsearchStore) LookupContentHashes(_ context.Context, repoID, branch string, nodeIDs []string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build a lookup set for requested nodeIDs.
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
