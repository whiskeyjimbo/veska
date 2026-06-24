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

// Options carries usearch HNSW build tunables resolved from the storage config
// profile. It is backend-agnostic so the config/builder layer can
// construct it without importing the hnsw_native-only UsearchStore; the memvec
// backend ignores it. The zero value reproduces the historical defaults
// (ExpansionAdd=indexExpansionAdd, single-threaded build).
type Options struct {
	// ExpansionAdd is the HNSW construction beam (ef_construction): higher =
	// better recall, slower build. 0 means "use the package default".
	ExpansionAdd uint
	// BuildThreads caps the goroutines fanned across idx.Add during a batch
	// build. 0 or 1 means serial (deterministic). >1 enables the parallel build
	// (faster, nondeterministic graph - calibrated via eval-usearch-profile).
	BuildThreads uint
}

// Option mutates an Options. Functional options keep the NewVectorStorage call
// site readable and match the domain-constructor convention used elsewhere.
type Option func(*Options)

// WithExpansionAdd sets the HNSW construction beam (ef_construction).
func WithExpansionAdd(ef uint) Option { return func(o *Options) { o.ExpansionAdd = ef } }

// WithBuildThreads sets the parallel-build fan-out width (1 = serial).
func WithBuildThreads(n uint) Option { return func(o *Options) { o.BuildThreads = n } }

// NewVectorStorage constructs a VectorStorage implementation for the specified backend. For
// BackendUsearch, the dir argument specifies the home directory from which persisted
// index files are loaded; opts tune the HNSW build (ignored by memvec).
func NewVectorStorage(kind BackendKind, dir string, opts ...Option) (ports.VectorStorage, error) {
	switch kind {
	case BackendMemory, "":
		return memvec.New(), nil
	case BackendUsearch:
		var o Options
		for _, fn := range opts {
			fn(&o)
		}
		return Open(dir, o)
	default:
		return nil, fmt.Errorf("vector: unknown backend kind %q (want %q or %q)",
			kind, BackendMemory, BackendUsearch)
	}
}
