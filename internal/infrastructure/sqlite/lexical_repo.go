// This file implements ports.LexicalSearcher on top of the m3.03.2 FTS
// pair (node_fts_words + node_fts_trigrams). Results are fused with
// Reciprocal Rank Fusion (RRF) at query time.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/tokenize"
)

// rrfK is the standard RRF dampener: 1 / (rrfK + rank). 60 is the value
// originally proposed by Cormack et al. and the de-facto default in
// production lexical+vector fusion. Lower values amplify top hits, higher
// values flatten the ranking.
const rrfK = 60.0

// rrfFetchMultiplier is how many candidate rows we pull from EACH arm
// before fusing. Pulling more than k from either arm lets the RRF score
// surface results that rank well in both arms even if neither ranks them
// at the very top.
const rrfFetchMultiplier = 4

// LexicalRepo is a SQLite-backed implementation of ports.LexicalSearcher.
// It reads from the two FTS5 virtual tables created by migration 0007 and
// fuses their results with Reciprocal Rank Fusion.
type LexicalRepo struct {
	readDB *sql.DB
}

// NewLexicalRepo constructs a LexicalRepo backed by readDB.
func NewLexicalRepo(readDB *sql.DB) *LexicalRepo {
	return &LexicalRepo{readDB: readDB}
}

// Search returns up to k LexicalHits for query in (repoID, branch).
//
// The query string is pre-tokenised with tokenize.Symbol so a camelCase
// query like "closeFinding" is also matchable as "close Finding". The
// trigram arm uses the flattened original query (built-in trigram tokenizer
// requires no help from the caller).
//
// Empty query short-circuits to nil; k <= 0 short-circuits to nil.
func (r *LexicalRepo) Search(ctx context.Context, repoID, branch, query string, k int) ([]ports.LexicalHit, error) {
	if k <= 0 || query == "" {
		return nil, nil
	}

	// Build the FTS5 MATCH expressions. The words arm consumes the
	// pre-tokenised form so the caller-side split is symmetric with the
	// write path (Promoter writes tokenize.Symbol of kind+symbol+name).
	// The trigram arm consumes the raw query — trigram tokenizer needs no
	// pre-processing.
	wordsExpr := buildWordsMatchExpr(query)
	rawExpr := buildTrigramMatchExpr(query)

	limit := k * rrfFetchMultiplier

	// Score map keyed by node_id; populated from both arms.
	score := make(map[string]float64)

	if wordsExpr != "" {
		if err := r.accumulate(ctx, score, `
			SELECT node_id FROM node_fts_words
			WHERE repo_id = ? AND branch = ? AND words MATCH ?
			ORDER BY rank LIMIT ?`,
			repoID, branch, wordsExpr, limit,
		); err != nil {
			return nil, fmt.Errorf("lexical: words query: %w", err)
		}
	}

	if rawExpr != "" {
		if err := r.accumulate(ctx, score, `
			SELECT node_id FROM node_fts_trigrams
			WHERE repo_id = ? AND branch = ? AND raw MATCH ?
			ORDER BY rank LIMIT ?`,
			repoID, branch, rawExpr, limit,
		); err != nil {
			return nil, fmt.Errorf("lexical: trigrams query: %w", err)
		}
	}

	if len(score) == 0 {
		return nil, nil
	}

	// Convert map to a slice and pick the top k by score.
	hits := make([]ports.LexicalHit, 0, len(score))
	for id, s := range score {
		hits = append(hits, ports.LexicalHit{NodeID: id, Score: s})
	}
	// Partial sort: cheap selection sort over a tiny set (typically a few
	// dozen rows max). Avoids importing sort for a constant-bounded slice
	// and keeps the hot path allocation-free beyond the result slice.
	if len(hits) > k {
		topKByScore(hits, k)
		hits = hits[:k]
	} else {
		topKByScore(hits, len(hits))
	}
	return hits, nil
}

// accumulate runs an ORDER BY rank query and adds 1/(rrfK+rank) to score
// for each returned node_id (rank starts at 0 for the first row).
func (r *LexicalRepo) accumulate(
	ctx context.Context,
	score map[string]float64,
	query string,
	args ...any,
) error {
	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	rank := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		score[id] += 1.0 / (rrfK + float64(rank))
		rank++
	}
	return rows.Err()
}

// topKByScore reorders hits so that the highest-score n entries occupy the
// front of the slice. It is a selection-style partial sort; stable order is
// not promised among ties (FTS5 already breaks them by rank inside each
// arm, and the RRF fusion is what we care about here).
func topKByScore(hits []ports.LexicalHit, n int) {
	if n > len(hits) {
		n = len(hits)
	}
	for i := 0; i < n; i++ {
		best := i
		for j := i + 1; j < len(hits); j++ {
			if hits[j].Score > hits[best].Score {
				best = j
			}
		}
		if best != i {
			hits[i], hits[best] = hits[best], hits[i]
		}
	}
}

// buildWordsMatchExpr produces an FTS5 MATCH expression for the words arm.
// Each token from tokenize.Symbol becomes a prefix-match term joined by OR
// so any single hit raises the score. Empty input yields "".
func buildWordsMatchExpr(query string) string {
	tokens := splitFields(tokenize.Symbol(query))
	return joinFTS5OR(tokens, true /* withPrefix */)
}

// buildTrigramMatchExpr produces an FTS5 MATCH expression for the trigram
// arm. The trigram tokenizer needs at least 3 characters in a token to
// match anything; we feed the raw query collapsed to alnum runs so very
// short queries still hit when possible.
func buildTrigramMatchExpr(query string) string {
	// For trigram, FTS5 matches substrings of length >= 3. Quote the
	// query as a phrase so spaces inside the query are interpreted as
	// "must appear in sequence". A single token longer than 2 chars is
	// the common case.
	q := flattenForTrigram(query)
	if len(q) < 3 {
		return ""
	}
	return `"` + escapeFTS5Quote(q) + `"`
}

// flattenForTrigram returns query with any embedded quote removed (FTS5
// MATCH strings cannot contain `"` even when quoted). Other characters are
// passed through unchanged — the trigram tokenizer accepts any UTF-8.
func flattenForTrigram(query string) string {
	out := make([]byte, 0, len(query))
	for i := 0; i < len(query); i++ {
		if query[i] == '"' {
			continue
		}
		out = append(out, query[i])
	}
	return string(out)
}

// escapeFTS5Quote doubles any embedded `"`; defensive — flattenForTrigram
// already strips them — but kept so future changes that allow quotes don't
// silently produce invalid FTS5 syntax.
func escapeFTS5Quote(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// splitFields splits s on whitespace into non-empty tokens.
func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' || s[i] == '\t' || s[i] == '\n' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	return out
}

// joinFTS5OR joins tokens with " OR ", optionally appending `*` to each
// for prefix matching. Tokens are double-quoted to be safe against any
// FTS5 reserved characters. Returns "" if tokens is empty.
func joinFTS5OR(tokens []string, withPrefix bool) string {
	if len(tokens) == 0 {
		return ""
	}
	out := make([]byte, 0, 32)
	for i, t := range tokens {
		// Skip pure-symbol or empty tokens — they cannot contribute.
		if t == "" {
			continue
		}
		if i > 0 && len(out) > 0 {
			out = append(out, ' ', 'O', 'R', ' ')
		}
		out = append(out, '"')
		out = append(out, escapeFTS5Quote(t)...)
		out = append(out, '"')
		if withPrefix {
			out = append(out, '*')
		}
	}
	return string(out)
}
