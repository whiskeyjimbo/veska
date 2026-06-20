// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// LexicalHit is the result returned by a LexicalSearcher. The score is the Reciprocal Rank
// Fusion total over the words- and trigrams-FTS indices; callers must preserve
// the ordering imposed by the adapter and not interpret the absolute value.
type LexicalHit struct {
	NodeID string
	Score  float64
}

// LexicalSearcher is the fallback port consulted when the embedder is unreachable.
// The adapter combines results from words and trigram FTS5 indices via Reciprocal
// Rank Fusion and returns the top k results. k must be greater than zero. An empty
// query string returns an empty slice with no error.
type LexicalSearcher interface {
	Search(ctx context.Context, repoID, branch, query string, k int) ([]LexicalHit, error)
}
