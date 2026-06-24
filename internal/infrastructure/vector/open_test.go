// SPDX-License-Identifier: AGPL-3.0-only

// Package vector_test verifies the stub build path for Open.
package vector_test

import (
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// TestOpen_StubReturnsErrVectorStoreUnavailable confirms that calling Open without the
// hnsw_native build tag returns ErrVectorStoreUnavailable. When the hnsw_native tag
// is present, this test is skipped because the native path is covered by open_native_test.go.
func TestOpen_StubReturnsErrVectorStoreUnavailable(t *testing.T) {
	if isNativeBuild() {
		t.Skip("skipping stub test: hnsw_native build tag is active")
	}

	store, err := vector.Open(t.TempDir(), vector.Options{})
	if store != nil {
		t.Errorf("expected nil store from stub path, got %T", store)
	}
	if !errors.Is(err, vector.ErrVectorStoreUnavailable) {
		t.Errorf("expected ErrVectorStoreUnavailable, got %v", err)
	}
}
