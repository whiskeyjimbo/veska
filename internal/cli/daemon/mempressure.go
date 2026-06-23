// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"log/slog"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/platform/meminfo"
)

// Memory floors for the in-memory (memvec) vector backend. memvec
// holds every vector in RAM via linear scan, so a heavy promotion burst plus a
// concurrent query fan-out can drive the daemon into OOM. These two consts let
// the daemon react before that happens.
//
// advisoryFloorBytes (startup) >= pressureFloorBytes (runtime) by design: we
// want to warn the operator early, while still only throttling work once memory
// is genuinely tight. The values are deliberately conservative defaults, not
// tuned config:
//   - advisory ~2 GiB: a small repo plus its embeddings fits well under this;
//     below it, a large repo's in-memory vectors risk crowding out the OS, so
//     the operator should consider VESKA_VECTOR_BACKEND=usearch.
//   - pressure ~512 MiB: at this point the kernel has little free headroom, so
//     skipping a queue/embed tick to let memory recover is cheaper than risking
//     an OOM kill mid-transaction.
const (
	advisoryFloorBytes uint64 = 2 << 30   // 2 GiB
	pressureFloorBytes uint64 = 512 << 20 // 512 MiB
)

// availMemFunc reports available system RAM in bytes; ok is false when the
// reading is unsupported on this platform (e.g. non-linux), in which case
// callers MUST degrade to a no-op rather than assume any memory level.
type availMemFunc func() (uint64, bool)

// defaultAvailMem wraps the platform meminfo leaf, mapping its error into the
// ok=false "unknown" signal the daemon's checks expect.
func defaultAvailMem() (uint64, bool) {
	n, err := meminfo.Available()
	if err != nil {
		return 0, false
	}
	return n, true
}

// underMemoryPressure reports whether available RAM has dropped below the
// runtime pressure floor. It is called from the queue/embedder poll loops every
// ~250ms across goroutines; the underlying read is a stateless syscall with no
// shared mutable state, so it is safe to call concurrently. A nil reader or an
// unsupported platform (ok=false) degrades to false (never pause).
func underMemoryPressure(avail availMemFunc, floorBytes uint64) bool {
	if avail == nil {
		return false
	}
	n, ok := avail()
	if !ok {
		return false
	}
	return n < floorBytes
}

// resolveFloorBytes maps the operator's [storage] memory_pressure_floor_mib into
// bytes, falling back to the built-in pressureFloorBytes when unset (<= 0).
func resolveFloorBytes(cfgMiB int) uint64 {
	if cfgMiB > 0 {
		return uint64(cfgMiB) << 20
	}
	return pressureFloorBytes
}

// maybeWarnLowMemory emits a one-shot WARN when the memvec backend is elected
// and available RAM is below the advisory floor, advising the operator to
// switch backends or free memory. It mirrors the static-embedder WARN in
// electEmbedder. Non-memory backends, ample memory, and unsupported readers are
// all silent (degrade safely).
func maybeWarnLowMemory(backend vector.BackendKind, avail availMemFunc, logger *slog.Logger) {
	if backend != vector.BackendMemory || avail == nil {
		return
	}
	n, ok := avail()
	if !ok || n >= advisoryFloorBytes {
		return
	}
	logger.Warn("daemon: low available memory for in-memory vector backend - consider VESKA_VECTOR_BACKEND=usearch or freeing memory",
		"available_bytes", n, "advisory_floor_bytes", advisoryFloorBytes)
}

// pressureGate wraps underMemoryPressure with edge-triggered logging so the
// memory-pressure throttle on the post-promotion queue lanes is diagnosable
// rather than a silent indefinite stall (solov2-b5aw). busy() is polled every
// ~250ms from the queue poller goroutine; it logs only on the rising edge
// (pressure engages) and the falling edge (clears), never per-tick. active is
// guarded so concurrent callers stay race-free.
type pressureGate struct {
	avail  availMemFunc
	floor  uint64
	logger *slog.Logger
	mu     sync.Mutex
	active bool
}

func newPressureGate(avail availMemFunc, floorBytes uint64, logger *slog.Logger) *pressureGate {
	return &pressureGate{avail: avail, floor: floorBytes, logger: logger}
}

// busy reports whether memory pressure should defer the deferrable lanes, and
// logs the transition into/out of that state.
func (g *pressureGate) busy() bool {
	under := underMemoryPressure(g.avail, g.floor)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case under && !g.active:
		g.active = true
		n, _ := g.avail()
		g.logger.Warn("daemon: memory pressure - deferring post-promotion queue lanes (embedding continues); free RAM or switch to VESKA_VECTOR_BACKEND=usearch",
			"available_mib", n>>20, "floor_mib", g.floor>>20)
	case !under && g.active:
		g.active = false
		g.logger.Info("daemon: memory pressure cleared - resuming post-promotion queue lanes")
	}
	return under
}
