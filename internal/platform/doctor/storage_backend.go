// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import "github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"

// StorageBackendParams holds the inputs for CheckStorageBackend.
// Callers obtain VectorCount from the active VectorStorage implementation
// (e.g. memvec.Store.TotalVectorCount).
type StorageBackendParams struct {
	// Backend is the active backend name: "memory" or "usearch".
	Backend string
	// VectorCount is the total number of stored vectors.
	VectorCount int
}

// StorageBackendReport is the result of CheckStorageBackend.
type StorageBackendReport struct {
	// Backend is the active backend name.
	Backend string `json:"backend"`
	// VectorCount is the total number of stored vectors.
	VectorCount int `json:"vector_count"`
	// CeilingWarning is "" (healthy), "yellow" (≥75k for the memory backend),
	// or "red" (≥90k for the memory backend). Always "" for the usearch backend.
	CeilingWarning string `json:"ceiling_warning"`
}

// CheckStorageBackend inspects the active VectorStorage backend and returns
// a report including backend name, vector count, and any ceiling warnings.
// Ceiling warnings apply only to the in-memory backend:
//
//	yellow: VectorCount ≥ memvec.YellowThreshold (75k)
//	red: VectorCount > memvec.RedThreshold (90k)
func CheckStorageBackend(params StorageBackendParams) StorageBackendReport {
	report := StorageBackendReport{
		Backend:     params.Backend,
		VectorCount: params.VectorCount,
	}

	if params.Backend == "memory" {
		switch {
		case params.VectorCount > memvec.RedThreshold:
			report.CeilingWarning = "red"
		case params.VectorCount >= memvec.YellowThreshold:
			report.CeilingWarning = "yellow"
		}
	}

	return report
}
