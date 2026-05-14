package doctor

import "github.com/whiskeyjimbo/veska/internal/infrastructure/vector/sqlitevec"

// StorageBackendParams holds the inputs for CheckStorageBackend.
// Callers obtain VectorCount from the active VectorStorage implementation
// (e.g. sqlitevec.SQLiteVecStore.TotalVectorCount).
type StorageBackendParams struct {
	// Backend is the active backend name: "sqlite-vec" or "usearch".
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
	// CeilingWarning is "" (healthy), "yellow" (≥75k for sqlite-vec), or
	// "red" (≥90k for sqlite-vec).  Always "" for the usearch backend.
	CeilingWarning string `json:"ceiling_warning"`
}

// CheckStorageBackend inspects the active VectorStorage backend and returns
// a report including backend name, vector count, and any ceiling warnings.
//
// Ceiling warnings apply only to the sqlite-vec backend:
//   - yellow: VectorCount ≥ sqlitevec.SQLiteVecYellowThreshold (75k)
//   - red:    VectorCount >  sqlitevec.SQLiteVecRedThreshold   (90k)
func CheckStorageBackend(params StorageBackendParams) StorageBackendReport {
	report := StorageBackendReport{
		Backend:     params.Backend,
		VectorCount: params.VectorCount,
	}

	if params.Backend == "sqlite-vec" {
		switch {
		case params.VectorCount > sqlitevec.SQLiteVecRedThreshold:
			report.CeilingWarning = "red"
		case params.VectorCount >= sqlitevec.SQLiteVecYellowThreshold:
			report.CeilingWarning = "yellow"
		}
	}

	return report
}
