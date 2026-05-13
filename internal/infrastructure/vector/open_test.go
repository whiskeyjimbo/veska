// Package vector_test verifies the stub build path for Open.
package vector_test

import (
	"errors"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/vector"
)

// TestOpen_StubReturnsErrVectorStoreUnavailable confirms that without the
// hnsw_native build tag, Open returns ErrVectorStoreUnavailable.
// When the hnsw_native tag IS present this test is skipped — the native path
// is exercised by open_native_test.go.
func TestOpen_StubReturnsErrVectorStoreUnavailable(t *testing.T) {
	if isNativeBuild() {
		t.Skip("skipping stub test: hnsw_native build tag is active")
	}

	store, err := vector.Open(t.TempDir())
	if store != nil {
		t.Errorf("expected nil store from stub path, got %T", store)
	}
	if !errors.Is(err, vector.ErrVectorStoreUnavailable) {
		t.Errorf("expected ErrVectorStoreUnavailable, got %v", err)
	}
}
