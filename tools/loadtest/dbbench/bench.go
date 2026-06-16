//go:build eval

// Package dbbench benchmarks Go SQLite drivers (mattn, zombiezen) against
// the workloads veska's storage layer actually runs. See README.md and
// for the motivation.
// The package is build-tagged `eval` so `go test./.` stays fast. The
// mattn driver additionally requires cgo; its registration lives in a
// `cgo`-tagged file and is silently absent when cgo is disabled.
package dbbench

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Bench is the per-driver bench facade. One Bench owns one open database;
// the harness opens a fresh Bench per driver per run.
// Workload methods receive the iteration index `i` so they can choose
// well-distributed inputs (random-but-deterministic) without per-call
// allocation overhead inside the hot loop.
type Bench interface {
	Name() string
	Open(ctx context.Context, path string) error
	ApplySchema(ctx context.Context, stmts []string) error
	Seed(ctx context.Context, cfg SeedConfig) error

	// Read workloads.
	GraphRead(ctx context.Context, i int) error
	FTSQuery(ctx context.Context, i int) error
	RehydrateScan(ctx context.Context) (rows int, err error)
	QueuePoll(ctx context.Context, i int) error

	// Write workloads.
	PromotionTx(ctx context.Context, i int) error
	BulkIngest(ctx context.Context, batchSize int, i int) error

	Close() error
}

// Factory builds a fresh Bench for a single run.
type Factory func() Bench

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a driver factory under the given name. Each driver file
// (driver_mattn.go / driver_zombiezen.go) calls this from its init.
func Register(name string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// Drivers returns the names of every registered driver, sorted.
func Drivers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NewBench instantiates a registered driver by name.
func NewBench(name string) (Bench, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("dbbench: unknown driver %q (registered: %v)", name, driverNames())
	}
	return f(), nil
}

func driverNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
