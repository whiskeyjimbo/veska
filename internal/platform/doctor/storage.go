// Package doctor provides health-check and diagnostic utilities for veska.
package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// StorageReport holds filesystem metrics for the veska data directory,
// matching the storage data schema.
type StorageReport struct {
	VeskaHome       string   `json:"veska_home"`
	DBPath          string   `json:"db_path"`
	DBSizeBytes     int64    `json:"db_size_bytes"`
	WALSizeBytes    int64    `json:"wal_size_bytes"`
	HNSWIndexPaths  []string `json:"hnsw_index_paths"`
	HNSWSizeBytes   int64    `json:"hnsw_size_bytes"`
	HNSWVectorCount int64    `json:"hnsw_vector_count"`
	FreeBytes       int64    `json:"free_bytes"`
	FreeRatio       float64  `json:"free_ratio"`
}

// sidecarRows is the minimal subset of the JSON sidecar we need to count vectors.
// We avoid importing the hnsw_native package entirely.
type sidecarRows struct {
	Rows map[string]json.RawMessage `json:"rows"`
}

// ComputeFreeRatio returns free / (free + db + wal + hnsw).
// Returns 1.0 when the denominator is zero to avoid division by zero.
func ComputeFreeRatio(freeBytes, dbBytes, walBytes, hnswBytes int64) float64 {
	denom := freeBytes + dbBytes + walBytes + hnswBytes
	if denom == 0 {
		return 1.0
	}
	return float64(freeBytes) / float64(denom)
}

// fileSize returns the size of the file at path, or 0 if the file does not exist
// or cannot be stat'd. Missing files are not an error.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// CheckStorage reads the filesystem under veskaHome and returns a populated
// StorageReport. It never requires a running UsearchStore - all data is derived
// from on-disk files.
func CheckStorage(veskaHome string) (StorageReport, error) {
	dbPath := filepath.Join(veskaHome, "veska.db")
	walPath := filepath.Join(veskaHome, "veska.db-wal")

	dbSize := fileSize(dbPath)
	walSize := fileSize(walPath)

	// Collect all *.hnsw files.
	hnswPaths, err := filepath.Glob(filepath.Join(veskaHome, "*.hnsw"))
	if err != nil {
		return StorageReport{}, err
	}
	sort.Strings(hnswPaths)

	var hnswSizeBytes int64
	var hnswVectorCount int64

	for _, p := range hnswPaths {
		hnswSizeBytes += fileSize(p)

		// Matching sidecar: same base,.json extension.
		sidecarPath := p[:len(p)-len(".hnsw")] + ".json"
		data, err := os.ReadFile(sidecarPath)
		if err != nil {
			// Sidecar missing or unreadable - skip vector count for this index.
			continue
		}
		var sc sidecarRows
		if err := json.Unmarshal(data, &sc); err != nil {
			// Malformed sidecar - skip rather than error.
			continue
		}
		hnswVectorCount += int64(len(sc.Rows))
	}

	freeBytes := getFreeBytes(veskaHome)

	return StorageReport{
		VeskaHome:       veskaHome,
		DBPath:          dbPath,
		DBSizeBytes:     dbSize,
		WALSizeBytes:    walSize,
		HNSWIndexPaths:  hnswPaths,
		HNSWSizeBytes:   hnswSizeBytes,
		HNSWVectorCount: hnswVectorCount,
		FreeBytes:       freeBytes,
		FreeRatio:       ComputeFreeRatio(freeBytes, dbSize, walSize, hnswSizeBytes),
	}, nil
}
