// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package vector

import (
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"
)

// BackendKind identifies the available VectorStorage implementations.
// - 'memory' is the default in-memory linear-scan store with no external dependencies.
// - 'usearch' is the HNSW-based scale backend requiring libusearch_c.so and the hnsw_native build tag.
type BackendKind string

const (
	// BackendMemory selects the default in-memory linear-scan backend.
	BackendMemory BackendKind = "memory"

	// BackendUsearch selects the HNSW vector backend, requiring libusearch_c.so at runtime.
	BackendUsearch BackendKind = "usearch"
)

// NewVectorStorage constructs a VectorStorage implementation for the specified backend. For
// BackendUsearch, the dir argument specifies the home directory from which persisted
// index files are loaded.
func NewVectorStorage(kind BackendKind, dir string) (ports.VectorStorage, error) {
	switch kind {
	case BackendMemory, "":
		return memvec.New(), nil
	case BackendUsearch:
		return Open(dir)
	default:
		return nil, fmt.Errorf("vector: unknown backend kind %q (want %q or %q)",
			kind, BackendMemory, BackendUsearch)
	}
}
