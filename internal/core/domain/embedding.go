// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

// EmbeddingRow is a single vector embedding record tied to a graph node.
type EmbeddingRow struct {
	NodeID      string
	ContentHash string
	ModelID     string
	Vector      []float32
}

// SearchHit is a single result from a vector similarity search.
type SearchHit struct {
	NodeID string
	Score  float32
}

// VectorFilter constrains a vector search to a subset of stored embeddings.
// Zero-value string fields are treated as "no constraint" (match all).
type VectorFilter struct {
	RepoID  string
	Branch  string
	ModelID string
}
