// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// openNative creates a UsearchStore and, only when veskaHome holds a
// clean-shutdown marker, loads the persisted HNSW snapshot from it. The marker
// is consumed (removed) on every open so a crash later this session is never
// mistaken for a clean exit. Without the marker (crash or first boot) an empty
// store is returned and the daemon rebuilds it from node_embeddings - this is
// what keeps the index free of vectors for nodes deleted while the daemon was
// down. A corrupt/partial snapshot is dropped (not fatal) so startup falls back
// to the same SQL rebuild rather than bricking.
func openNative(veskaHome string, opts Options) (ports.VectorStorage, error) {
	store, err := NewUsearchStore(opts)
	if err != nil {
		return nil, err
	}
	if veskaHome == "" {
		return store, nil
	}
	markerPath := filepath.Join(veskaHome, cleanShutdownMarker)
	if _, statErr := os.Stat(markerPath); statErr != nil {
		return store, nil // no clean-shutdown snapshot: rebuild from SQL
	}
	_ = os.Remove(markerPath)
	if err := store.Load(veskaHome); err != nil {
		slog.Warn("vector: load persisted index failed; rebuilding from node_embeddings", "err", err)
		store.Destroy() // drop any partially-loaded indexes; store stays usable
		return store, nil
	}
	store.hydratedFromDisk = true
	return store, nil
}

// UsearchAvailable reports whether the usearch backend is compiled in. True in
// this native build (hnsw_native tag); a missing libusearch_c.so still surfaces
// at Open time, which BackendAuto handles by falling back to memvec.
func UsearchAvailable() bool { return true }
