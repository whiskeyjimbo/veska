// This file implements ports.LexicalSearcher on top of the m3.03.2 FTS
// pair (node_fts_words + node_fts_trigrams). Results are fused with
// Reciprocal Rank Fusion (RRF) at query time.

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/tokenize"
)

// rrfK is the standard Reciprocal Rank Fusion dampener. The value of 60.0 is
// standard in lexical and vector fusion to balance top hits against tail
// rankings.
const rrfK = 60.0

// rrfFetchMultiplier controls the candidate pool size fetched from each ranking
// source. Pulling more than k candidates from each arm allows Reciprocal Rank
// Fusion to surface items that perform moderately well across both sources.
const rrfFetchMultiplier = 4

// LexicalRepo implements ports.LexicalSearcher using separate words and
// trigrams SQLite FTS5 tables, fusing their rankings at query time with
// Reciprocal Rank Fusion.
type LexicalRepo struct {
	readDB *sql.DB
}

// NewLexicalRepo constructs a LexicalRepo.
func NewLexicalRepo(readDB *sql.DB) *LexicalRepo {
	return &LexicalRepo{readDB: readDB}
}

// Search performs a combined words and trigrams search, fusing hits using
// Reciprocal Rank Fusion. The query string is pre-tokenized for the words
// matcher to align with the write path, whereas the trigram matcher queries
// the raw input directly.
func (r *LexicalRepo) Search(ctx context.Context, repoID, branch, query string, k int) ([]ports.LexicalHit, error) {
	if k <= 0 || query == "" {
		return nil, nil
	}

	wordsExpr := buildWordsMatchExpr(query)
	rawExpr := buildTrigramMatchExpr(query)

	limit := k * rrfFetchMultiplier

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

	hits := make([]ports.LexicalHit, 0, len(score))
	for id, s := range score {
		hits = append(hits, ports.LexicalHit{NodeID: id, Score: s})
	}
	// A selection-based partial sort is used on the small candidate set to
	// avoid the overhead of the standard sort package and keep the execution
	// allocation-free.
	if len(hits) > k {
		topKByScore(hits, k)
		hits = hits[:k]
	} else {
		topKByScore(hits, len(hits))
	}
	return hits, nil
}

// accumulate aggregates Reciprocal Rank Fusion scores from a query into the
// cumulative score map.
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

// topKByScore performs a selection-style partial sort to place the
// highest-scoring elements at the front of the slice.
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

// buildWordsMatchExpr constructs an FTS5 MATCH query for the words index,
// converting each token into an OR-joined prefix match.
func buildWordsMatchExpr(query string) string {
	tokens := splitFields(tokenize.Symbol(query))
	return joinFTS5OR(tokens, true /* withPrefix*/)
}

// buildTrigramMatchExpr constructs an FTS5 MATCH query for the trigram index.
// Because trigram indexes require tokens of at least length 3 to match, shorter
// inputs yield empty queries.
func buildTrigramMatchExpr(query string) string {
	q := flattenForTrigram(query)
	if len(q) < 3 {
		return ""
	}
	return `"` + escapeFTS5Quote(q) + `"`
}

// flattenForTrigram strips double quotes from the input because SQLite FTS5
// MATCH expressions cannot contain double quotes.
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

// escapeFTS5Quote doubles embedded double quotes to ensure valid FTS5 syntax.
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

func joinFTS5OR(tokens []string, withPrefix bool) string {
	if len(tokens) == 0 {
		return ""
	}
	out := make([]byte, 0, 32)
	for i, t := range tokens {
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
