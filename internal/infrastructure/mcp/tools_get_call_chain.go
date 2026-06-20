// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
)

// chainedSelectorCallRe matches call expressions containing a selector chain of at least two dots (e.g. `a.b.c(`), which are not modeled as edges by the legacy parser.
var chainedSelectorCallRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*){2,}\s*\(`)

// seedBodyContainsChainedSelector reports whether the seed body contains a chained selector call, treating empty bodies conservatively as a match to trigger the unresolved hint.
func seedBodyContainsChainedSelector(body string) bool {
	if body == "" {
		return true
	}
	return chainedSelectorCallRe.MatchString(body)
}

type getCallChainParams struct {
	NodeID          string `json:"node_id"`
	Symbol          string `json:"symbol"`
	RepoID          string `json:"repo_id"`
	Branch          string `json:"branch"`
	Depth           int    `json:"depth"`
	ExpandCrossRepo bool   `json:"expand_cross_repo"`
	// Direction selects which CALLS edges to traverse ("out", "in", or "both").
	Direction string `json:"direction"`
}

const maxCallChainDepth = 10

func makeGetCallChainHandler(graph ports.GraphReader, resolve ResolveFunc, resolveInbound InboundResolveFunc, repos application.RepoLister, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getCallChainParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		depth := p.Depth
		if depth <= 0 {
			depth = 3 // default
		}
		if depth > maxCallChainDepth {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("depth %d exceeds maximum of %d", depth, maxCallChainDepth)}
		}

		repoID, branch, nid, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.NodeID, p.Symbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID, p.Branch, p.NodeID = repoID, branch, nid
		canonical, ok := normalizeDirection(p.Direction)
		if !ok {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid direction %q (want in|out|both or callers|callees|both)", p.Direction)}
		}
		dirOut, dirIn := true, false // empty defaults to outbound (callees)
		switch canonical {
		case "in":
			dirOut, dirIn = false, true
		case "both":
			dirOut, dirIn = true, true
		}

		g, err := graph.LoadGraph(ctx, p.RepoID, p.Branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("load graph failed: %v", err)}
		}

		startID := domain.NodeID(p.NodeID)
		visited := make(map[domain.NodeID]bool)
		visitedEdges := make(map[string]bool)
		var resultNodes []*domain.Node
		var resultEdges []*domain.Edge
		var crossRepoEdges []CrossRepoEdge

		type bfsItem struct {
			id   domain.NodeID
			hops int
		}
		queue := []bfsItem{{id: startID, hops: 0}}
		visited[startID] = true

		for len(queue) > 0 {
			item := queue[0]
			queue = queue[1:]

			// Cross-repo stubs are resolved for each visited node based on the direction of traversal.
			if resolve != nil && dirOut {
				resolved, resolveErr := resolve(ctx, string(item.id), p.Branch, p.ExpandCrossRepo)
				if resolveErr == nil {
					for _, re := range resolved {
						crossRepoEdges = append(crossRepoEdges, CrossRepoEdge{
							SrcNodeID: re.SrcNodeID,
							DstNodeID: re.DstNodeID,
							DstRepoID: re.DstRepoID,
							DstBranch: re.DstBranch,
							Kind:      re.Kind,
							CrossRepo: true,
							SrcLine:   re.SrcLine,
						})
					}
				}

			}
			if resolveInbound != nil && dirIn {
				resolved, resolveErr := resolveInbound(ctx, string(item.id), p.Branch)
				if resolveErr == nil {
					for _, re := range resolved {
						crossRepoEdges = append(crossRepoEdges, CrossRepoEdge{
							SrcNodeID: re.SrcNodeID,
							DstNodeID: re.DstNodeID,
							DstRepoID: re.DstRepoID,
							DstBranch: re.DstBranch,
							Kind:      re.Kind,
							CrossRepo: true,
							SrcLine:   re.SrcLine,
						})
					}
				}
			}

			if item.hops >= depth {
				continue
			}

			if dirOut {
				for _, e := range g.OutgoingEdges(item.id) {
					if e.Kind != domain.EdgeCalls {
						continue
					}
					if !visitedEdges[e.ID] {
						visitedEdges[e.ID] = true
						resultEdges = append(resultEdges, e)
					}
					if !visited[e.Tgt] {
						visited[e.Tgt] = true
						if n, ok := g.Node(e.Tgt); ok {
							resultNodes = append(resultNodes, n)
						}
						queue = append(queue, bfsItem{id: e.Tgt, hops: item.hops + 1})
					}
				}
			}
			if dirIn {
				for _, e := range g.IncomingEdges(item.id) {
					if e.Kind != domain.EdgeCalls {
						continue
					}
					// CALLS edges originating from structural container nodes (e.g. packages) are filtered out here to ensure only function-level callers are returned.
					if src, ok := g.Node(e.Src); ok && !isCallableKind(src.Kind) {
						continue
					}
					if !visitedEdges[e.ID] {
						visitedEdges[e.ID] = true
						resultEdges = append(resultEdges, e)
					}
					if !visited[e.Src] {
						visited[e.Src] = true
						if n, ok := g.Node(e.Src); ok {
							resultNodes = append(resultNodes, n)
						}
						queue = append(queue, bfsItem{id: e.Src, hops: item.hops + 1})
					}
				}
			}
		}

		// Degraded reasons distinguish between unresolved chained selectors and external/stdlib callees that are not indexed.
		reasons := []string{}
		var indexing []string
		if len(resultEdges) == 0 && len(crossRepoEdges) == 0 && dirOut {
			if seed, ok := g.Node(startID); ok {
				switch seed.Kind {
				case domain.KindFunction, domain.KindMethod:
					// snippet fetch is best-effort - on error we keep the
					// conservative legacy reason rather than swallow the
					// failure into a different signal.
					body, snipErr := graph.GetNodeSnippet(ctx, p.RepoID, p.Branch, startID)
					if snipErr != nil || seedBodyContainsChainedSelector(body) {
						reasons = append(reasons, protocol.DegradedReasonChainedSelectorsUnresolved)
					} else {
						reasons = append(reasons, protocol.DegradedReasonExternalCalleesOnly)
					}
				}
			}
			// Empty responses during an active cold scan include the indexing degraded reason.
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}

		reconciling := reconcilingForRepos(reconcile, []string{p.RepoID})
		if len(reconciling) > 0 {
			reasons = append(reasons, protocol.DegradedReasonWakeReconciling)
		}
		return callChainResponse{
			Nodes:                nodesToDTO(resultNodes),
			Edges:                edgesToDTO(resultEdges),
			CrossRepoEdges:       crossRepoEdges,
			IndexingRepos:        indexing,
			IncludedStaging:      false,
			DegradedReasons:      reasons,
			WakeReconcilingRepos: reconciling,
		}, nil
	}
}

// isCallableKind reports whether a node kind represents a callable symbol rather than a structural container.
func isCallableKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindPackage, domain.KindFile, domain.KindModule, domain.KindChunk:
		return false
	default:
		return true
	}
}
