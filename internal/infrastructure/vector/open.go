package vector

import (
	"errors"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrVectorStoreUnavailable is returned by Open when the usearch native library is not compiled in or cannot be loaded.
var ErrVectorStoreUnavailable = errors.New(
	"usearch native library not available: rebuild with -tags hnsw_native " +
		"and ensure libusearch_c.so is on LD_LIBRARY_PATH (Linux) or DYLD_LIBRARY_PATH (macOS)")

// Open initializes the VectorStorage for the given veskaHome directory. When compiled with
// the hnsw_native build tag, this function creates a UsearchStore, loads any persisted indexes
// from veskaHome, and returns the store ready for use. The returned ports.VectorStorage
// is a *UsearchStore; callers that need to call Destroy must assert the concrete type.
// When compiled without the hnsw_native build tag, this function always returns ErrVectorStoreUnavailable.
func Open(veskaHome string) (ports.VectorStorage, error) {
	return openNative(veskaHome)
}
