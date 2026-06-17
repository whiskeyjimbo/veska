// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// TestOpen_NativeEmptyDirSucceeds verifies that calling Open on an empty or non-existent
// directory succeeds when compiled with the hnsw_native tag. In this case, no index files
// are loaded, resulting in an empty but fully functional store.
func TestOpen_NativeEmptyDirSucceeds(t *testing.T) {
	dir := t.TempDir()
	store, err := vector.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) returned unexpected error: %v", dir, err)
	}
	if store == nil {
		t.Fatal("Open returned nil store without error")
		return
	}
	// The caller is responsible for calling Destroy to free CGo resources by casting the
	// returned interface to a *UsearchStore. No cleanup is performed in this test because
	// the returned interface does not expose Destroy.
}
