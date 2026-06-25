// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
)

type typeHierarchyParams struct {
	NodeID string `json:"node_id"`
	Symbol string `json:"symbol"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	Depth  int    `json:"depth"`
}

const maxTypeHierarchyDepth = 10

// typeHierarchyResponse mirrors callChainResponse: nodes + edges plus the
// standard degraded-reason envelope so callers handle cold-scan/reconcile the
// same way across graph tools.
type typeHierarchyResponse struct {
	Nodes           []nodeDTO `json:"nodes"`
	Edges           []edgeDTO `json:"edges"`
	IncludedStaging bool      `json:"included_staging"`
	DegradedReasons []string  `json:"degraded_reasons"`
	IndexingRepos   []string  `json:"indexing_repos,omitempty"`
}

// makeFindImplementationsHandler answers "what implements this interface" or,
// when the seed is a concrete type, "what interfaces does this type satisfy".
// Direction is inferred from the seed kind rather than a param: an interface
// seed walks INCOMING IMPLEMENTS edges (implementers point at it); a
// type/struct seed walks OUTGOING IMPLEMENTS edges (it points at the interfaces
// it satisfies).
func makeFindImplementationsHandler(graph ports.GraphReader, repos application.RepoLister, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p typeHierarchyParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		repoID, branch, nid, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.NodeID, p.Symbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		g, err := graph.LoadGraph(ctx, repoID, branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("load graph failed: %v", err)}
		}
		startID := domain.NodeID(nid)
		seed, ok := g.Node(startID)
		if !ok {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node not found: %s", nid)}
		}

		var resultNodes []*domain.Node
		var resultEdges []*domain.Edge
		emit := func(e *domain.Edge, other domain.NodeID) {
			if e.Kind != domain.EdgeImplements {
				return
			}
			resultEdges = append(resultEdges, e)
			if n, ok := g.Node(other); ok {
				resultNodes = append(resultNodes, n)
			}
		}
		// An interface is satisfied BY types (incoming); a concrete type
		// satisfies interfaces (outgoing). Anything else has no implements
		// relationship and returns empty.
		if seed.Kind == domain.KindInterface {
			for _, e := range g.IncomingEdges(startID) {
				emit(e, e.Src)
			}
		} else {
			for _, e := range g.OutgoingEdges(startID) {
				emit(e, e.Tgt)
			}
		}

		reasons := degradedForTypeQuery(scans, reconcile, repoID, len(resultEdges))
		indexing, _ := indexingRepoIDs(scans)
		return typeHierarchyResponse{
			Nodes:           nodesToDTO(resultNodes),
			Edges:           edgesToDTO(resultEdges),
			DegradedReasons: reasons,
			IndexingRepos:   indexingForReasons(reasons, indexing),
		}, nil
	}
}

// makeGetTypeHierarchyHandler returns the IMPLEMENTS + EMBEDS neighborhood of a
// seed type in both directions, depth-bounded. It is the type-relationship
// analogue of eng_get_call_chain: a BFS that surfaces the local subtype/embed
// structure so a caller sees, in one query, what a type embeds, what embeds it,
// and which interfaces sit above or below it.
func makeGetTypeHierarchyHandler(graph ports.GraphReader, repos application.RepoLister, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p typeHierarchyParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		depth := p.Depth
		if depth <= 0 {
			depth = 3
		}
		if depth > maxTypeHierarchyDepth {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("depth %d exceeds maximum of %d", depth, maxTypeHierarchyDepth)}
		}
		repoID, branch, nid, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.NodeID, p.Symbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		g, err := graph.LoadGraph(ctx, repoID, branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("load graph failed: %v", err)}
		}

		startID := domain.NodeID(nid)
		if _, ok := g.Node(startID); !ok {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node not found: %s", nid)}
		}
		visited := map[domain.NodeID]bool{startID: true}
		visitedEdges := map[string]bool{}
		var resultNodes []*domain.Node
		var resultEdges []*domain.Edge

		type bfsItem struct {
			id   domain.NodeID
			hops int
		}
		queue := []bfsItem{{id: startID, hops: 0}}
		for len(queue) > 0 {
			item := queue[0]
			queue = queue[1:]
			if item.hops >= depth {
				continue
			}
			step := func(e *domain.Edge, next domain.NodeID) {
				if !isTypeHierarchyEdge(e.Kind) {
					return
				}
				if !visitedEdges[e.ID] {
					visitedEdges[e.ID] = true
					resultEdges = append(resultEdges, e)
				}
				if !visited[next] {
					visited[next] = true
					if n, ok := g.Node(next); ok {
						resultNodes = append(resultNodes, n)
					}
					queue = append(queue, bfsItem{id: next, hops: item.hops + 1})
				}
			}
			for _, e := range g.OutgoingEdges(item.id) {
				step(e, e.Tgt)
			}
			for _, e := range g.IncomingEdges(item.id) {
				step(e, e.Src)
			}
		}

		reasons := degradedForTypeQuery(scans, reconcile, repoID, len(resultEdges))
		indexing, _ := indexingRepoIDs(scans)
		return typeHierarchyResponse{
			Nodes:           nodesToDTO(resultNodes),
			Edges:           edgesToDTO(resultEdges),
			DegradedReasons: reasons,
			IndexingRepos:   indexingForReasons(reasons, indexing),
		}, nil
	}
}

// isTypeHierarchyEdge reports whether an edge participates in the type
// hierarchy (embedding or interface satisfaction).
func isTypeHierarchyEdge(k domain.EdgeKind) bool {
	return k == domain.EdgeImplements || k == domain.EdgeEmbeds
}

// degradedForTypeQuery surfaces the indexing/reconcile degraded reasons that the
// other graph tools report, only when the result is empty (an empty answer
// during a cold scan or wake reconcile is "retry", not "no relationship").
func degradedForTypeQuery(scans ScanTrackerReader, reconcile ReconcileReader, repoID string, edgeCount int) []string {
	reasons := []string{}
	if edgeCount == 0 {
		if _, busy := indexingRepoIDs(scans); busy {
			reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
		}
	}
	if len(reconcilingForRepos(reconcile, []string{repoID})) > 0 {
		reasons = append(reasons, protocol.DegradedReasonWakeReconciling)
	}
	return reasons
}

// indexingForReasons returns the indexing repo list only when the indexing
// reason is present, keeping the envelope consistent with the other tools.
func indexingForReasons(reasons, indexing []string) []string {
	if slices.Contains(reasons, protocol.DegradedReasonIndexingInProgress) {
		return indexing
	}
	return nil
}
