// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// rrfConstant is the standard Reciprocal Rank Fusion smoothing term.
// 60 is the value the original Cormack et al. paper recommends and what
// most production hybrid-retrieval systems (including semble) use. It
// dampens the dominance of rank-1 so a candidate that appears at rank
// 3 in both lists outranks one that appears at rank 1 in only one list.
const rrfConstant = 60

// rrfFuse merges vector hits and lexical hits with Reciprocal Rank
// Fusion. The merged score per node is sum over each retriever of
//
//	1 / (rrfConstant + rank_in_that_retriever)
//
// where rank is 1-indexed. Nodes appearing in only one list still
// contribute (so the fusion never demotes a strong unique hit below a
// weak common one). Returns the top-k by fused score. The Score field
// on each returned domain.SearchHit holds the fused score so downstream
// callers (e.g. rerank) can scale relative to a sensible max.
// k <= 0 means "no truncation, return all fused candidates" - used by
// the Semantic path so the post-fusion rerank has full visibility.
func rrfFuse(vec []domain.SearchHit, lex []ports.LexicalHit, k int) []domain.SearchHit {
	if len(vec) == 0 && len(lex) == 0 {
		return nil
	}
	scores := make(map[string]float32, len(vec)+len(lex))
	for rank, h := range vec {
		scores[h.NodeID] += 1.0 / float32(rrfConstant+rank+1)
	}
	for rank, h := range lex {
		scores[h.NodeID] += 1.0 / float32(rrfConstant+rank+1)
	}
	out := make([]domain.SearchHit, 0, len(scores))
	for id, s := range scores {
		out = append(out, domain.SearchHit{NodeID: id, Score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].NodeID < out[j].NodeID
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

func tokenizeQuery(q string) []string {
	fields := strings.Fields(strings.ToLower(q))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Strip surrounding punctuation, skip tokens shorter than 3
		// chars - words like "a", "to", "of" produce too many spurious
		// matches.
		f = strings.Trim(f, `.,;:!?"'()[]{}`)
		if len(f) < 3 {
			continue
		}
		out = append(out, f)
	}
	return out
}
