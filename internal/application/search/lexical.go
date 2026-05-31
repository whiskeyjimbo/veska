package search

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Lexical performs a pure FTS5 lookup (words+trigrams RRF fusion) without
// touching the embedder. Intended for callers that have already decided
// the lexical path is the right one (e.g. an explicit /lexical tool or a
// caller that wants deterministic substring matching). For the
// embedder-fallback case, prefer Semantic — it tags the response with
// degraded_reasons so the agent knows the reasoning mode changed.
func (s *Service) Lexical(ctx context.Context, repoID, branch, query string, k int) ([]Result, error) {
	if k <= 0 || s.lexical == nil {
		return nil, nil
	}
	return s.lexicalFallback(ctx, repoID, branch, query, k)
}

// lexicalFallback runs LexicalSearcher.Search and hydrates the hits via
// NodeLookup. It is the shared body of the Semantic-fallback path and
// the explicit Lexical method.
func (s *Service) lexicalFallback(ctx context.Context, repoID, branch, query string, k int) ([]Result, error) {
	hits, err := s.lexical.Search(ctx, repoID, branch, query, k)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}
	if len(hits) == 0 {
		return []Result{}, nil
	}

	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.NodeID
	}

	metas, err := s.nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return nil, fmt.Errorf("node lookup: %w", err)
	}
	byID := make(map[string]ports.NodeMeta, len(metas))
	for _, m := range metas {
		byID[m.NodeID] = m
	}

	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		m, ok := byID[h.NodeID]
		if !ok {
			continue
		}
		out = append(out, Result{
			NodeID:     h.NodeID,
			Score:      float32(h.Score),
			SymbolPath: m.SymbolPath,
			FilePath:   m.FilePath,
			Kind:       m.Kind,
			LineStart:  m.LineStart,
			LineEnd:    m.LineEnd,
			Snippet:    m.Snippet,
		})
	}
	return out, nil
}
