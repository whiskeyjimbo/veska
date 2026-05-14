package vector

import (
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/sqlitevec"
)

// BackendKind names the available VectorStorage implementations.
//
// sqlite-vec is the default: zero extra native dependencies, linear scan,
// adequate for workspaces below sqlitevec.SQLiteVecYellowThreshold (75k) nodes.
//
// usearch is the scale backend: requires libusearch_c.so and the hnsw_native
// build tag; delivers HNSW with float16 quantization, recall@10=0.9870 @50k,
// p95=1.90ms @50k and 4.28ms @250k.
type BackendKind string

const (
	// BackendSQLiteVec selects the in-memory linear-scan backend (default).
	// No external shared libraries required.
	BackendSQLiteVec BackendKind = "sqlite-vec"

	// BackendUsearch selects the usearch HNSW backend.
	// Requires the hnsw_native build tag and libusearch_c.so at runtime.
	BackendUsearch BackendKind = "usearch"
)

// NewVectorStorage constructs the VectorStorage for the specified backend.
//
// For BackendSQLiteVec the dir argument is unused (the store is in-memory).
// For BackendUsearch dir is the veskaHome directory used to Load persisted
// HNSW index files; if dir is empty no persisted state is loaded.
func NewVectorStorage(kind BackendKind, dir string) (ports.VectorStorage, error) {
	switch kind {
	case BackendSQLiteVec, "":
		return sqlitevec.New(), nil
	case BackendUsearch:
		return Open(dir)
	default:
		return nil, fmt.Errorf("vector: unknown backend kind %q (want %q or %q)",
			kind, BackendSQLiteVec, BackendUsearch)
	}
}
