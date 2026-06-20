// SPDX-License-Identifier: AGPL-3.0-only

//go:build !hnsw_native

package vector_test

// isNativeBuild reports whether the hnsw_native build tag is active.
// This stub returns false.
func isNativeBuild() bool { return false }
