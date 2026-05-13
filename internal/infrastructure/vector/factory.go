// Package vector provides VectorStorage implementations for the engram solov2 module.
//
// The usearch-backed implementation is compiled only when the hnsw_native build tag
// is present (CGo + libusearch_c.so required). This file anchors the package for
// builds and tests that do not enable the native tag.
package vector
