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

	// BackendAuto defers the choice to ElectVectorBackend: usearch for large
	// graphs (when compiled in), memvec otherwise. It must be resolved to a
	// concrete kind before reaching NewVectorStorage.
	BackendAuto BackendKind = "auto"
)

// AutoElectThreshold is the per-(repo,branch) ready-vector count at or above
// which BackendAuto elects usearch over memvec. Rationale: memvec autolink is
// O(n^2) (one query/node x O(n) linear scan) while embedding is O(n); the two
// cross near ~12K nodes, so above this autolink dominates and grows
// quadratically. usearch's O(log n) search is net-faster end-to-end from here up
// (measured ~0.999 recall, query p95 2-4x faster; see eval-usearch-ab). 10K sits
// at the knee with margin. Distinct from memvec.YellowThreshold (75K), the harder
// "memvec strained" line.
const AutoElectThreshold = 10_000

// ElectVectorBackend resolves the concrete backend to use. An explicit
// memory/usearch choice is returned unchanged. BackendAuto elects usearch only
// when the largest single (repo,branch) index is at/above AutoElectThreshold AND
// usearch is compiled in; otherwise memvec. The result is never BackendAuto.
func ElectVectorBackend(configured BackendKind, maxRepoVectors int, usearchAvailable bool) BackendKind {
	if configured != BackendAuto {
		return configured
	}
	if usearchAvailable && maxRepoVectors >= AutoElectThreshold {
		return BackendUsearch
	}
	return BackendMemory
}

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
