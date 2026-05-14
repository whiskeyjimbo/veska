package ports

import "context"

// LexicalHit is the minimal projection a LexicalSearcher returns: an
// identified node and the fused score. The score is the Reciprocal Rank
// Fusion total over the words- and trigrams-FTS indices (higher = better);
// callers preserve the ordering imposed by the adapter and do not interpret
// the absolute value.
type LexicalHit struct {
	NodeID string
	Score  float64
}

// LexicalSearcher is the port the application-layer search service consults
// when the embedder is unreachable (m3.03.2 fallback) — and the only port
// involved on that path other than the existing NodeLookup hydrator. The
// adapter is expected to combine results from the words FTS5 index (built-in
// unicode61 over pre-tokenised symbol text) and the trigram FTS5 index
// (built-in trigram over the raw symbol text) via RRF and return the top k.
//
// k > 0 is required; k <= 0 is a programmer error and the adapter may
// short-circuit to an empty result. An empty query string is treated as a
// no-op: an empty slice is returned with nil error.
type LexicalSearcher interface {
	Search(ctx context.Context, repoID, branch, query string, k int) ([]LexicalHit, error)
}
