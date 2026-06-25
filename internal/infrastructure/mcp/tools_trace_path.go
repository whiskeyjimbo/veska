// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/tracepath"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

type tracePathParams struct {
	FromNodeID string   `json:"from_node_id"`
	FromSymbol string   `json:"from_symbol"`
	ToNodeID   string   `json:"to_node_id"`
	ToSymbol   string   `json:"to_symbol"`
	EdgeKinds  []string `json:"edge_kinds"`
	MaxDepth   int      `json:"max_depth"`
	MaxPaths   int      `json:"max_paths"`
	RepoID     string   `json:"repo_id"`
	Branch     string   `json:"branch"`
}

const (
	defaultTracePathDepth = 12
	maxTracePathDepth     = 25
	defaultTracePathPaths = 1
	maxTracePathPaths     = 25
	// tracePathHubDegree gates expansion through high-fan-out nodes; tracePathMaxVisited
	// bounds total exploration. Both mirror the blast-radius defaults.
	tracePathHubDegree  = 50
	tracePathMaxVisited = 10000
)

// tracePathDTO is one connecting route: an ordered node list with the edges
// between consecutive nodes (len(Edges) == len(Nodes)-1).
type tracePathDTO struct {
	Nodes []nodeDTO `json:"nodes"`
	Edges []edgeDTO `json:"edges"`
}

// tracePathResponse is the envelope for eng_trace_path. Paths is empty when no
// route was found within the bounds, in which case Reason explains why (an empty
// result is not an error). Truncated/Bound report when a bound stopped the
// search early.
type tracePathResponse struct {
	Paths           []tracePathDTO `json:"paths"`
	Truncated       bool           `json:"truncated,omitempty"`
	Bound           string         `json:"bound,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	DegradedReasons []string       `json:"degraded_reasons"`
	IndexingRepos   []string       `json:"indexing_repos,omitempty"`
}

// makeTracePathHandler answers "how does A reach B": it returns the shortest
// path(s) from the source to the target over the chosen edge kinds (CALLS by
// default). It complements eng_get_call_chain (single-source flood) and
// eng_get_blast_radius (transitive closure) by answering a directed
// point-to-point question. MVP is single-repo; cross-repo stub hopping (the
// ResolveFunc seam call_chain uses) is a future extension.
func makeTracePathHandler(graph ports.GraphReader, repos application.RepoLister, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p tracePathParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}

		depth := p.MaxDepth
		if depth <= 0 {
			depth = defaultTracePathDepth
		}
		if depth > maxTracePathDepth {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("max_depth %d exceeds maximum of %d", depth, maxTracePathDepth)}
		}
		maxPaths := p.MaxPaths
		if maxPaths <= 0 {
			maxPaths = defaultTracePathPaths
		}
		if maxPaths > maxTracePathPaths {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("max_paths %d exceeds maximum of %d", maxPaths, maxTracePathPaths)}
		}

		// Resolve the source (handles repo/branch discovery from cwd), then resolve
		// the target within the SAME repo+branch. Either side rejects an ambiguous
		// symbol with a "pass node_id" message, consistent with the other tools.
		repoID, branch, fromID, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.FromNodeID, p.FromSymbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		_, _, toID, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, repoID, branch, p.ToNodeID, p.ToSymbol)
		if rpcErr != nil {
			return nil, rpcErr
		}

		g, err := graph.LoadGraph(ctx, repoID, branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("load graph failed: %v", err)}
		}
		if _, ok := g.Node(domain.NodeID(fromID)); !ok {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("from node not found: %s", fromID)}
		}
		if _, ok := g.Node(domain.NodeID(toID)); !ok {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("to node not found: %s", toID)}
		}

		res := tracepath.Find(g, domain.NodeID(fromID), domain.NodeID(toID), tracepath.Options{
			EdgeKinds:  parseEdgeKinds(p.EdgeKinds),
			MaxDepth:   depth,
			MaxPaths:   maxPaths,
			HubDegree:  tracePathHubDegree,
			MaxVisited: tracePathMaxVisited,
		})

		paths := make([]tracePathDTO, 0, len(res.Paths))
		for _, pth := range res.Paths {
			nodes := make([]*domain.Node, 0, len(pth.Nodes))
			for _, id := range pth.Nodes {
				if n, ok := g.Node(id); ok {
					nodes = append(nodes, n)
				}
			}
			paths = append(paths, tracePathDTO{Nodes: nodesToDTO(nodes), Edges: edgesToDTO(pth.Edges)})
		}

		reasons := degradedForTypeQuery(scans, reconcile, repoID, len(res.Paths))
		indexing, _ := indexingRepoIDs(scans)
		return tracePathResponse{
			Paths:           paths,
			Truncated:       res.Truncated,
			Bound:           res.Bound,
			Reason:          res.Reason,
			DegradedReasons: reasons,
			IndexingRepos:   indexingForReasons(reasons, indexing),
		}, nil
	}
}

// parseEdgeKinds maps the requested kind strings to EdgeKinds, defaulting to
// CALLS when none are given. Input is upper-cased to tolerate lower-case callers
// (edge kinds are stored upper-case).
func parseEdgeKinds(in []string) []domain.EdgeKind {
	if len(in) == 0 {
		return []domain.EdgeKind{domain.EdgeCalls}
	}
	out := make([]domain.EdgeKind, 0, len(in))
	for _, k := range in {
		if k == "" {
			continue
		}
		out = append(out, domain.EdgeKind(strings.ToUpper(k)))
	}
	if len(out) == 0 {
		return []domain.EdgeKind{domain.EdgeCalls}
	}
	return out
}
