// Package vector_test verifies the vector package compiles without native libs.
package vector_test

import (
	// Import the package to ensure it compiles without hnsw_native.
	_ "github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)
