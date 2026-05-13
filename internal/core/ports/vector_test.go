//go:build hnsw_native

// Package ports_test verifies that the usearch infrastructure adapter satisfies
// the VectorStorage interface at compile time.
package ports_test

import (
	"github.com/whiskeyjimbo/engram/solov2/internal/core/ports"
	"github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/vector"
)

// Compile-time interface satisfaction check.
var _ ports.VectorStorage = (*vector.UsearchStore)(nil)
