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

	// parallelAddMin is the batch size below which the parallel build falls back
	// to a serial loop - goroutine fan-out overhead outweighs the gain on tiny
	// batches (e.g. the ~32-row cold-scan embed lane). Only big-batch boot
	// rehydrate (~13k+) clears this and pays for the fan-out.
	parallelAddMin = 256
)

// indexKey uniquely identifies an HNSW index partitioned by repository, branch, and model.
type indexKey struct {
	repoID  string
	branch  string
	modelID string
}

// rowMeta stores the Go-side metadata associated with each usearch slot. The original
// float32 vector is omitted because usearch maintains its own copy;
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

// newIndexEntry builds one native HNSW index. expansionAdd is the construction
// beam (ef_construction); buildThreads, when >1, tells usearch to size per-thread
// scratch buffers so concurrent idx.Add calls (the parallel build path) are safe.
func newIndexEntry(expansionAdd, buildThreads uint) (*indexEntry, error) {
	conf := usearchlib.IndexConfig{
		Dimensions: indexDim,
		Metric:     usearchlib.L2sq,
		// F32, not F16: measured on the real graph, F16's per-insert quantization
		// made the index build ~8x slower (20.8s vs 2.6s at ~13k nodes) and lowered
		// autolink recall vs the exact memvec oracle (0.9992 -> 0.9995 going F16->F32).
		// F16's only win was ~2x less index RAM - a non-issue below the 75k
		// YellowThreshold (memvec already holds float32). Revisit F16 only at
		// multi-million-vector scale.
		Quantization:    usearchlib.F32,
		Connectivity:    indexConnectivity,
		ExpansionAdd:    expansionAdd,
		ExpansionSearch: indexExpansionSearch,
	}
	idx, err := usearchlib.NewIndex(conf)
	if err != nil {
		return nil, fmt.Errorf("usearch: new index: %w", err)
	}
	if buildThreads > 1 {
		// Cap usearch's internal add-threading to our fan-out width so it
		// allocates the right number of per-thread scratch buffers; without
		// this, concurrent idx.Add from N goroutines is unsafe.
		if err := idx.ChangeThreadsAdd(buildThreads); err != nil {
			_ = idx.Destroy()
			return nil, fmt.Errorf("usearch: change threads add %d: %w", buildThreads, err)
		}
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

// prepareAdd does the serial, Go-side bookkeeping for inserting one row and
// returns the numeric id the caller must idx.Add the vector under. Because the
// underlying usearch index has no in-place update, any pre-existing entry is
// untracked here (its slot becomes a tombstone) and a brand-new id is assigned.
// The actual idx.Add is deliberately NOT done here so a batch can fan the Adds
// out concurrently after every id is assigned (see UpsertEmbeddings). Per-row
// capacity is already covered by the batch pre-reserve in UpsertEmbeddings.
func (e *indexEntry) prepareAdd(row domain.EmbeddingRow) uint64 {
	if oldID, exists := e.nodeToID[row.NodeID]; exists {
		delete(e.rows, oldID)
	}
	id := e.nextID
	e.nextID++
	e.rows[id] = rowMeta{NodeID: row.NodeID, ContentHash: row.ContentHash, ModelID: row.ModelID}
	e.nodeToID[row.NodeID] = id
	return id
}

// UsearchStore implements the VectorStorage interface using separate, in-memory usearch
// HNSW indexes partitioned by repository, branch, and model. It uses float32 storage
// and is safe for concurrent access.
type UsearchStore struct {
	mu      sync.RWMutex
	indexes map[indexKey]*indexEntry

	// expansionAdd and buildThreads are the resolved build tunables applied to
	// every index this store creates. Defaults reproduce historical
	// behavior: indexExpansionAdd and serial (1) build.
	expansionAdd uint
	buildThreads uint
}

// DeleteNodes drops the given node_ids from every index matching (repoID,
// branch). It untracks them in the metadata maps - the same mechanism upsert
// uses for replacement - so they no longer surface in Search; the native HNSW
// slot is left as a tombstone (the usearch API has no delete). Unknown ids and
// an empty slice are no-ops.
func (s *UsearchStore) DeleteNodes(_ context.Context, repoID, branch string, nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.indexes {
		if k.repoID != repoID || k.branch != branch {
			continue
		}
		for _, nid := range nodeIDs {
			if id, ok := e.nodeToID[nid]; ok {
				delete(e.rows, id)
				delete(e.nodeToID, nid)
			}
		}
	}
	return nil
}

// MemoryUsage reports the total resident bytes of the native HNSW indexes
// (float32 vectors + graph), as accounted by usearch itself - the honest
// C-side footprint, which Go's HeapAlloc cannot see. Go-side metadata maps are
// excluded. Used by the backend-metrics eval harness.
func (s *UsearchStore) MemoryUsage() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total uint64
	for _, e := range s.indexes {
		u, err := e.idx.MemoryUsage()
		if err != nil {
			return 0, fmt.Errorf("usearch: memory usage: %w", err)
		}
		total += uint64(u)
	}
	return total, nil
}

// Compile-time check to ensure UsearchStore implements the ports.VectorStorage interface.
var _ ports.VectorStorage = (*UsearchStore)(nil)

// NewUsearchStore constructs an empty UsearchStore. opts.ExpansionAdd of 0
// falls back to indexExpansionAdd; opts.BuildThreads of 0 means serial build.
func NewUsearchStore(opts Options) (*UsearchStore, error) {
	ef := opts.ExpansionAdd
	if ef == 0 {
		ef = indexExpansionAdd
	}
	threads := opts.BuildThreads
	if threads == 0 {
		threads = 1
	}
	return &UsearchStore{
		indexes:      make(map[indexKey]*indexEntry),
		expansionAdd: ef,
		buildThreads: threads,
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
	e, err := newIndexEntry(s.expansionAdd, s.buildThreads)
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

	// Serial pre-pass: assign each row its numeric id, untrack any tombstoned
	// predecessor, and fill the Go-side maps. All mutation of indexEntry state
	// (nextID, rows, nodeToID) happens here, single-threaded, so the parallel
	// phase that follows touches ONLY the thread-safe C index.
	jobs := make([]addJob, 0, len(batch))
	for _, row := range batch {
		key := indexKey{repoID: repoID, branch: branch, modelID: row.ModelID}
		e := s.indexes[key]
		id := e.prepareAdd(row)
		jobs = append(jobs, addJob{e: e, id: id, vec: row.Vector})
	}

	// The expensive idx.Add calls run while s.mu is still held (deferred Unlock),
	// so Upsert stays mutually exclusive with Search exactly as before - the
	// fan-out parallelizes the Adds across cores, it does not widen concurrency.
	if s.buildThreads <= 1 || len(jobs) < parallelAddMin {
		for i := range jobs {
			if err := jobs[i].run(); err != nil {
				return err
			}
		}
		return nil
	}
	return runParallelAdds(jobs, int(s.buildThreads))
}

// addJob is one pre-assigned (index, id, vector) insertion. Splitting id/map
// assignment (done serially in prepareAdd) from the idx.Add lets the Adds fan
// out without racing on Go-side maps.
type addJob struct {
	e   *indexEntry
	id  uint64
	vec []float32
}

func (j *addJob) run() error {
	if err := j.e.idx.Add(j.id, j.vec); err != nil {
		return fmt.Errorf("usearch: add id=%d: %w", j.id, err)
	}
	return nil
}

// runParallelAdds fans the idx.Add calls across n goroutines over contiguous
// shards and returns the first error. Insertion order is nondeterministic, so
// the resulting HNSW graph differs run-to-run (equal quality, calibrated via
// eval-usearch-profile); this is why parallel build is opt-in.
func runParallelAdds(jobs []addJob, n int) error {
	if n > len(jobs) {
		n = len(jobs)
	}
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	shard := (len(jobs) + n - 1) / n
	for w := 0; w < n; w++ {
		lo := w * shard
		if lo >= len(jobs) {
			break
		}
		hi := lo + shard
		if hi > len(jobs) {
			hi = len(jobs)
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				if err := jobs[i].run(); err != nil {
					once.Do(func() { firstErr = err })
					return
				}
			}
		}(lo, hi)
	}
	wg.Wait()
	return firstErr
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
