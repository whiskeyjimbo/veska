package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)



type searchSimilarParams struct {
	NodeID string `json:"node_id"`
	// Symbol is an alias for node_id.
	Symbol string `json:"symbol"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// K is the neighbor count.
	K     int `json:"k,omitempty"`
	Limit int `json:"limit,omitempty"`
}

func makeSearchSimilarHandler(lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repos application.RepoLister, graph ports.GraphReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSimilarParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}

		repoID, branch, nid, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.NodeID, p.Symbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		p.Branch = branch
		p.NodeID = nid
		k, rpcErr := resolveK(p.K, p.Limit)
		if rpcErr != nil {
			return nil, rpcErr
		}

		results, rpcErr2 := findSimilarByNodeID(ctx, lookup, vectors, nodes, p.RepoID, p.Branch, p.NodeID, k)
		if rpcErr2 != nil {
			return nil, rpcErr2
		}
		return SearchResponse{Results: searchResultsToDTO(results), DegradedReasons: []string{}}, nil
	}
}

// findSimilarByNodeID runs a k-NN vector search using the embedding of a seed node and hydrates the matching nodes.
//
//nolint:revive,cyclop,funlen // see note above: verbatim relocation, diff-scoped gate
func findSimilarByNodeID(ctx context.Context, lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repoID, branch, nodeID string, k int) ([]search.Result, *RPCError) {
	hash, ready, err := lookup.ContentHashForNode(ctx, repoID, branch, nodeID)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: content hash lookup: %v", err)}
	}
	if !ready || hash == "" {
		return nil, &RPCError{
			Code:    CodeFailedPrecondition,
			Message: "node has no embedding",
			Data:    map[string]any{"reason": "node_not_embedded", "node_id": nodeID},
		}
	}
	blob, dim, found, err := lookup.LookupExisting(ctx, hash)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: embedding lookup: %v", err)}
	}
	if !found || dim == 0 {
		return nil, &RPCError{
			Code:    CodeFailedPrecondition,
			Message: "node has no embedding",
			Data:    map[string]any{"reason": "node_not_embedded", "node_id": nodeID},
		}
	}
	vec := veccodec.DecodeFloat32LE(blob, dim)


	hits, err := vectors.Search(ctx, repoID, branch, vec, k+1, domain.VectorFilter{})
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: vector search: %v", err)}
	}
	filtered := make([]domain.SearchHit, 0, len(hits))
	for _, h := range hits {
		if h.NodeID == nodeID {
			continue
		}
		filtered = append(filtered, h)
		if len(filtered) >= k {
			break
		}
	}

	ids := make([]string, len(filtered))
	for i, h := range filtered {
		ids[i] = h.NodeID
	}
	metas, err := nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("similar: node lookup: %v", err)}
	}
	byID := make(map[string]ports.NodeMeta, len(metas))
	for _, m := range metas {
		byID[m.NodeID] = m
	}
	out := make([]search.Result, 0, len(filtered))
	for _, h := range filtered {
		m, ok := byID[h.NodeID]
		if !ok {
			continue
		}
		out = append(out, search.Result{
			NodeID:     h.NodeID,
			Score:      h.Score,
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


var findRelatedInputSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Find symbols semantically similar to the code at a (file_path, line). The handler resolves the smallest enclosing node and reuses the eng_search_similar vector-neighbourhood path. Line is 1-indexed.",
  "properties": {
    "file_path": {"type": "string", "description": "Absolute path or repo-relative path to the file."},
    "line":      {"type": "integer", "minimum": 1, "description": "1-indexed source line; the enclosing node's embedding is the seed."},
    "repo_id":   {"type": "string"},
    "branch":    {"type": "string"},
    "k":         {"type": "integer", "minimum": 1, "description": "Neighbour count (default 10). 'limit' is accepted as an alias."},
    "limit":     {"type": "integer", "minimum": 1, "description": "Alias for k."},
    "cwd":       {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  },
  "required": ["file_path", "line"]
}`)

type findRelatedParams struct {
	FilePath string `json:"file_path"`
	Line     int    `json:"line"`
	RepoID   string `json:"repo_id"`
	Branch   string `json:"branch"`
	K        int    `json:"k,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// makeFindRelatedHandler resolves the target file path and line to the smallest enclosing node, then delegates to findSimilarByNodeID.
func makeFindRelatedHandler(lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findRelatedParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if p.FilePath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "file_path is required"}
		}
		if p.Line < 1 {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "line must be >= 1 (lines are 1-indexed)"}
		}
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}
		k, rpcErr := resolveK(p.K, p.Limit)
		if rpcErr != nil {
			return nil, rpcErr
		}


		p.FilePath = toStoredPath(ctx, repos, p.RepoID, p.FilePath)

		nodeID, rpcErr := resolveEnclosingNode(ctx, nodes, p.RepoID, p.Branch, p.FilePath, p.Line)
		if rpcErr != nil {
			return nil, rpcErr
		}

		results, rpcErr := findSimilarByNodeID(ctx, lookup, vectors, nodes, p.RepoID, p.Branch, nodeID, k)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return SearchResponse{Results: searchResultsToDTO(results), DegradedReasons: []string{}}, nil
	}
}

// resolveEnclosingNode identifies the smallest line-span node covering the specified line.
//
//nolint:revive // see note above: verbatim relocation, diff-scoped gate
func resolveEnclosingNode(ctx context.Context, nodes ports.NodeLookup, repoID, branch, filePath string, line int) (string, *RPCError) {
	ids, err := nodes.NodesInFile(ctx, repoID, branch, filePath)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("nodes in file: %v", err)}
	}
	if len(ids) == 0 {
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("no nodes indexed for file_path=%q in repo=%s; check that the file is part of a registered repo and has been promoted", filePath, repoID)}
	}
	metas, err := nodes.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("hydrate nodes: %v", err)}
	}
	bestID := ""
	bestSpan := math.MaxInt
	for _, m := range metas {
		if m.LineStart <= 0 || m.LineEnd <= 0 {
			continue
		}
		if line < m.LineStart || line > m.LineEnd {
			continue
		}
		span := m.LineEnd - m.LineStart + 1
		if span < bestSpan {
			bestSpan = span
			bestID = m.NodeID
		}
	}
	if bestID == "" {
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("no symbol or chunk covers %s:%d (line lies in whitespace or outside any indexed range)", filePath, line)}
	}
	return bestID, nil
}
