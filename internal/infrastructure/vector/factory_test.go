// SPDX-License-Identifier: AGPL-3.0-only

// Package vector_test verifies the vector package compiles without native libs.
package vector_test

import (
	// This side-effect import ensures the package compiles successfully on target
	// environments without enabling the hnsw_native build tag.
	_ "github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)
