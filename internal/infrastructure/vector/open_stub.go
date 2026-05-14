//go:build !hnsw_native

package vector

import "github.com/whiskeyjimbo/veska/internal/core/ports"

// openNative is the stub implementation. It always returns
// ErrVectorStoreUnavailable when the hnsw_native build tag is absent.
func openNative(_ string) (ports.VectorStorage, error) {
	return nil, ErrVectorStoreUnavailable
}
