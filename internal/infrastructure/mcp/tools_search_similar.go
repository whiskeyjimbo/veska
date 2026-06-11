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

// This file holds the eng_search_similar and eng_find_related handlers and
// their shared vector-neighbourhood core (findSimilarByNodeID). The tool
// registration and shared response types live in tools_search.go; the
// eng_search_semantic handler lives in tools_search_semantic.go.

type searchSimilarParams struct {
	NodeID string `json:"node_id"`
	// Symbol is an alias for node_id, resolved via GraphStorage.FindNodes.
	// Parity with eng_find_symbol / eng_get_call_chain / eng_get_blast_radius
	// . Ambiguous matches are rejected so the caller must
	// disambiguate via node_id.
	Symbol string `json:"symbol"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// K is the neighbour count. 'limit' accepted as an alias — see
	// searchSemanticParams for rationale .
	K     int `json:"k,omitempty"`
	Limit int `json:"limit,omitempty"`
}

func makeSearchSimilarHandler(lookup SimilarLookup, vectors ports.VectorStorage, nodes ports.NodeLookup, repos application.RepoLister, graph ports.GraphReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p searchSimilarParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ye6t: use the same cross-repo resolver as
		// eng_get_blast_radius / eng_get_context_pack / eng_find_symbol so
		// a bare `symbol` (or short node_id prefix) resolves across all
		// registered repos when repo_id is omitted. Before this, the
		// handler hard-required repo_id (via resolveRepoIDFromParams),
		// breaking parity with peer tools whose conventions block
		// promises symbol-only calls. resolveSeedOwner enforces the same
		// shape: node_id wins; ambiguous symbol matches are rejected so
		// the caller must disambiguate.
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

// findSimilarByNodeID is the shared core of eng_search_similar and
// eng_find_related . Given a seed node_id, it pulls the
// stored embedding, runs a k-NN vector search, filters the seed out,
// and hydrates the hits into search.Result records. The seed-filter
// over-requests by one neighbour so the caller still gets k results.
//
// Verbatim relocation: the (lookup, vectors, nodes) dependency trio plus the
// (repoID, branch, nodeID) seed predate the per-function arg/complexity gates,
// which are diff-scoped and only flag this because the file split makes git see
// the move as new code.
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

	// Over-request by one so we can filter the seed node out of results
	// and still return k neighbours (the seed is its own nearest match).
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

// findRelatedInputSchema declares the (file_path, line) anchor for the
// eng_find_related tool . Line is 1-indexed to match every
// other line-aware contract on the surface.
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

// makeFindRelatedHandler resolves (file_path, line) into the smallest
// enclosing node and delegates to findSimilarByNodeID. solov2-2g4r.
//
// "Smallest enclosing" handles TS-style nesting (class containing
// method) and intra-Go cases where a chunk and a function both cover
// the same line — picking the tightest span gives the agent the most
// specific embedding to anchor on. Chunks ARE eligible anchors because
// the user might point at a non-symbol region (a top-of-file comment,
// an init block, raw config) and "what else looks like this" is still
// a meaningful question there.
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

		// Node file_paths are stored repo-relative (ADR-S0017 §1). Normalise the
		// caller-supplied path to that form, mirroring eng_get_file_nodes, so an
		// absolute or relative path both match.
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

// resolveEnclosingNode picks the smallest line-span node whose range
// covers `line` in `filePath`. Returns CodeNotFound when no node
// matches (the file is unparsed, the line lies in pre-package
// whitespace, or the path doesn't belong to the repo). solov2-2g4r.
//
// Verbatim relocation: the (repoID, branch, filePath, line) anchor predates the
// per-function arg gate, which is diff-scoped and only flags this because the
// file split makes git see the move as new code.
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
