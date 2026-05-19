package vector_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

func TestNewVectorStorage_SQLiteVec(t *testing.T) {
	store, err := vector.NewVectorStorage(vector.BackendSQLiteVec, "")
	if err != nil {
		t.Fatalf("NewVectorStorage(sqlite-vec): %v", err)
	}
	if store == nil {
		t.Fatal("NewVectorStorage(sqlite-vec): returned nil store")
		return
	}
}

func TestNewVectorStorage_EmptyKindDefaultsSQLiteVec(t *testing.T) {
	store, err := vector.NewVectorStorage("", "")
	if err != nil {
		t.Fatalf("NewVectorStorage(empty): %v", err)
	}
	if store == nil {
		t.Fatal("NewVectorStorage(empty): returned nil store")
		return
	}
}

func TestNewVectorStorage_UnknownKindError(t *testing.T) {
	_, err := vector.NewVectorStorage("qdrant", "")
	if err == nil {
		t.Fatal("NewVectorStorage(unknown): expected error, got nil")
		return
	}
}

// TestNewVectorStorage_Usearch verifies that requesting the usearch backend
// without the hnsw_native build tag returns ErrVectorStoreUnavailable (the
// expected behaviour on a plain CI build).
func TestNewVectorStorage_Usearch_StubReturnsError(t *testing.T) {
	if isNativeBuild() {
		t.Skip("skipping stub-path test: hnsw_native build tag is active")
	}
	_, err := vector.NewVectorStorage(vector.BackendUsearch, t.TempDir())
	if err == nil {
		t.Fatal("NewVectorStorage(usearch) without hnsw_native: expected error, got nil")
		return
	}
}
