//go:build hnsw_native

package vector_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// TestOpen_NativeEmptyDirSucceeds verifies that Open on an empty/non-existent
// directory succeeds under the hnsw_native build tag — no .json files means
// no indexes are loaded and the store is empty but ready.
func TestOpen_NativeEmptyDirSucceeds(t *testing.T) {
	dir := t.TempDir()
	store, err := vector.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) returned unexpected error: %v", dir, err)
	}
	if store == nil {
		t.Fatal("Open returned nil store without error")
	}
	// Caller is responsible for Destroy; cast to *UsearchStore if needed.
	// Nothing to call here in the test as the interface does not expose Destroy.
}
