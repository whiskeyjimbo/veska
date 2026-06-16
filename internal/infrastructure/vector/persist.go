//go:build hnsw_native

package vector

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	usearchlib "github.com/unum-cloud/usearch/golang"
)

// sidecar is the JSON companion to each.hnsw file.
// It holds the Go-side metadata that usearch itself does not persist.
// Vectors are intentionally excluded — usearch holds the float16 copy.
type sidecar struct {
	RepoID   string             `json:"repoID"`
	Branch   string             `json:"branch"`
	ModelID  string             `json:"modelID"`
	Rows     map[uint64]rowMeta `json:"rows"`
	NodeToID map[string]uint64  `json:"nodeToID"`
	NextID   uint64             `json:"nextID"`
}

// sep is the field separator used in index filenames.
// url.QueryEscape encodes "|" as "%7C", so a literal "|" in a filename stem is
// always a separator — field values containing "|" are safely round-tripped.
const sep = "|"

// indexFileName encodes an indexKey into a safe filename stem.
// Format: vec-{repoID}|{branch}|{modelID}
// Each field is URL-query-escaped. The separator "|" is never emitted by
// url.QueryEscape (it would become "%7C"), so splitting on "|" is unambiguous
// regardless of what characters appear in the key fields (slashes, hyphens, etc.).
func indexFileName(key indexKey) string {
	escape := func(s string) string { return url.QueryEscape(s) }
	return fmt.Sprintf("vec-%s%s%s%s%s", escape(key.repoID), sep, escape(key.branch), sep, escape(key.modelID))
}

// parseIndexKey recovers an indexKey from a filename stem produced by indexFileName.
// Returns an error if the stem does not match the expected format.
func parseIndexKey(stem string) (indexKey, error) {
	if !strings.HasPrefix(stem, "vec-") {
		return indexKey{}, fmt.Errorf("persist: unexpected stem prefix in %q", stem)
	}
	rest := stem[len("vec-"):]
	parts := strings.SplitN(rest, sep, 3)
	if len(parts) != 3 {
		return indexKey{}, fmt.Errorf("persist: expected 3 fields in stem %q, got %d", stem, len(parts))
	}
	decode := func(s string) (string, error) { return url.QueryUnescape(s) }
	repoID, err := decode(parts[0])
	if err != nil {
		return indexKey{}, fmt.Errorf("persist: decode repoID in %q: %w", stem, err)
	}
	branch, err := decode(parts[1])
	if err != nil {
		return indexKey{}, fmt.Errorf("persist: decode branch in %q: %w", stem, err)
	}
	modelID, err := decode(parts[2])
	if err != nil {
		return indexKey{}, fmt.Errorf("persist: decode modelID in %q: %w", stem, err)
	}
	return indexKey{repoID: repoID, branch: branch, modelID: modelID}, nil
}

// Save persists all HNSW indexes plus their metadata sidecars to dir.
// For each (repoID, branch, modelID) index, two files are written:
//
//	<dir>/vec-{repoID}-{branch}-{modelID}.hnsw — native usearch index
//	<dir>/vec-{repoID}-{branch}-{modelID}.json — JSON sidecar (rows, nodeToID, nextID)
//
// Save acquires a read-lock for the duration of the iteration. Individual
// idx.Save calls go to disk while the lock is held; they do not mutate
// in-memory state.
func (s *UsearchStore) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persist: mkdir %q: %w", dir, err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for key, entry := range s.indexes {
		base := filepath.Join(dir, indexFileName(key))

		// Write native HNSW index.
		hnswPath := base + ".hnsw"
		if err := entry.idx.Save(hnswPath); err != nil {
			return fmt.Errorf("persist: save hnsw for %+v: %w", key, err)
		}

		// Write JSON sidecar.
		sc := sidecar{
			RepoID:   key.repoID,
			Branch:   key.branch,
			ModelID:  key.modelID,
			Rows:     entry.rows,
			NodeToID: entry.nodeToID,
			NextID:   entry.nextID,
		}
		data, err := json.Marshal(sc)
		if err != nil {
			return fmt.Errorf("persist: marshal sidecar for %+v: %w", key, err)
		}
		if err := os.WriteFile(base+".json", data, 0o644); err != nil {
			return fmt.Errorf("persist: write sidecar for %+v: %w", key, err)
		}
	}
	return nil
}

// Load reads all *.json sidecar files from dir, reconstructs the corresponding
// usearch HNSW index from the paired *.hnsw file, and adds each index to the store.
// Load acquires a write-lock for the duration of the operation.
// If the store already contains an index for a given key it is replaced.
func (s *UsearchStore) Load(dir string) error {
	jsonPaths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return fmt.Errorf("persist: glob %q: %w", dir, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, jsonPath := range jsonPaths {
		// Read and parse sidecar.
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			return fmt.Errorf("persist: read sidecar %q: %w", jsonPath, err)
		}
		var sc sidecar
		if err := json.Unmarshal(data, &sc); err != nil {
			return fmt.Errorf("persist: parse sidecar %q: %w", jsonPath, err)
		}

		key := indexKey{repoID: sc.RepoID, branch: sc.Branch, modelID: sc.ModelID}

		// Validate that the filename stem matches the embedded key fields.
		stem := strings.TrimSuffix(filepath.Base(jsonPath), ".json")
		if !strings.HasPrefix(stem, "vec-") {
			// Skip unrelated json files that may exist in the directory.
			continue
		}
		parsedKey, err := parseIndexKey(stem)
		if err != nil {
			return fmt.Errorf("persist: parse key from filename %q: %w", jsonPath, err)
		}
		if parsedKey != key {
			return fmt.Errorf("persist: filename key %+v does not match sidecar key %+v in %q", parsedKey, key, jsonPath)
		}

		// Destroy any existing entry for this key before loading.
		if existing, ok := s.indexes[key]; ok {
			_ = existing.idx.Destroy()
			delete(s.indexes, key)
		}

		// Create a fresh index and load the persisted HNSW data into it.
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
			return fmt.Errorf("persist: new index for %+v: %w", key, err)
		}

		hnswPath := strings.TrimSuffix(jsonPath, ".json") + ".hnsw"
		if err := idx.Load(hnswPath); err != nil {
			_ = idx.Destroy()
			return fmt.Errorf("persist: load hnsw %q: %w", hnswPath, err)
		}

		s.indexes[key] = &indexEntry{
			idx:      idx,
			rows:     sc.Rows,
			nodeToID: sc.NodeToID,
			nextID:   sc.NextID,
			capacity: 0, // will be recalculated on next Reserve call
		}
	}
	return nil
}
