// Package domain contains the core domain types for the veska module.
package domain

// EmbeddingRow is a single vector embedding record tied to a graph node.
type EmbeddingRow struct {
	NodeID      string
	ContentHash string
	ModelID     string
	Vector      []float32
}

// Hit is a single result from a vector similarity search.
type Hit struct {
	NodeID string
	Score  float32
}

// Filter constrains a vector search to a subset of stored embeddings.
// Zero-value string fields are treated as "no constraint" (match all).
type Filter struct {
	RepoID  string
	Branch  string
	ModelID string
}
