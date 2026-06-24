// SPDX-License-Identifier: AGPL-3.0-only

//go:build !hnsw_native

package vector

import "github.com/whiskeyjimbo/veska/internal/core/ports"

// openNative is the stub implementation. It always returns
// ErrVectorStoreUnavailable when the hnsw_native build tag is absent.
func openNative(_ string, _ Options) (ports.VectorStorage, error) {
	return nil, ErrVectorStoreUnavailable
}

// UsearchAvailable reports whether the usearch backend is compiled in. False in
// this stub build (no hnsw_native tag), so BackendAuto never elects usearch.
func UsearchAvailable() bool { return false }
