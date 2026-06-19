// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector

import "github.com/whiskeyjimbo/veska/internal/core/ports"

// openNative creates a UsearchStore, loads any persisted HNSW indexes from
// veskaHome, and returns the store ready for use.
// If veskaHome is empty or contains no vec-*.json files, Load is a no-op and
// an empty (but functional) store is returned.
// If Load fails, Destroy is called to release CGo memory before returning the
// error.
func openNative(veskaHome string) (ports.VectorStorage, error) {
	store, err := NewUsearchStore()
	if err != nil {
		return nil, err
	}
	if err := store.Load(veskaHome); err != nil {
		store.Destroy()
		return nil, err
	}
	return store, nil
}
