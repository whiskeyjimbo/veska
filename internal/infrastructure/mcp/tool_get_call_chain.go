package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ---------------------------------------------------------------------------
// eng_get_call_chain
// ---------------------------------------------------------------------------

type getCallChainParams struct {
	NodeID          string `json:"node_id"`
	Symbol          string `json:"symbol"`
	RepoID          string `json:"repo_id"`
	Branch          string `json:"branch"`
	Depth           int    `json:"depth"`
	ExpandCrossRepo bool   `json:"expand_cross_repo"`
	// Direction selects which CALLS edges to traverse: "out" (default —
	// callees, what this reaches), "in" (callers, what reaches this), or
	// "both". Default preserves prior behaviour; "in"/"both" close
	// solov2-2n33 where docs promised incoming traversal but only
	// outgoing was wired.
	Direction string `json:"direction"`
}

const maxCallChainDepth = 10

func makeGetCallChainHandler(graph ports.GraphStorage, resolve ResolveFunc, resolveInbound InboundResolveFunc, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getCallChainParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		// solov2-f0zt: when repo_id is omitted, fan-out across registered repos
		// to find which one owns the seed (node_id or symbol). Matches the
		// "default: fan out across registered repos" contract in `veska calls
		// --help`. resolveSeedOwner returns the (repo, branch, node_id) triple
		// in one call so we can drop the previous repo+branch+symbol-lookup
		// three-step.
		repoID, branch, nid, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.NodeID, p.Symbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID, p.Branch, p.NodeID = repoID, branch, nid
		depth := p.Depth
		if depth <= 0 {
			depth = 3 // default
		}
		if depth > maxCallChainDepth {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("depth %d exceeds maximum of %d", depth, maxCallChainDepth)}
		}
		dirOut, dirIn := true, false
		switch p.Direction {
		case "", "out":
			// defaults
		case "in":
			dirOut, dirIn = false, true
		case "both":
			dirOut, dirIn = true, true
		default:
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid direction %q (want out|in|both)", p.Direction)}
		}

		g, err := graph.LoadGraph(ctx, p.RepoID, p.Branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("load graph failed: %v", err)}
		}

		// BFS over CALLS edges starting from node_id.
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

			// Resolve cross-repo stubs for each visited node (including start).
			// Outbound resolution is gated by dirOut (the node is a caller);
			// inbound resolution by dirIn (the node is a callee). solov2-80hh
			// adds the inbound side for parity with eng_get_blast_radius.
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
						})
					}
				}
				// Silent miss: resolveErr != nil is ignored; continue BFS.
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
					// solov2-rkc5: skip CALLS edges that originate from a
					// package (or other non-callable container) node. The
					// extractor sometimes attaches a coarse "package calls
					// function" edge when it can't resolve the call site
					// to a specific function — surfacing those as
					// "callers" misleads the user, who expects function-
					// level call sites. The edge itself is preserved in
					// the graph for downstream tooling; we just don't
					// present it here.
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

		// solov2-jojv: emit a degraded_reasons hint when the seed is a
		// callable but no CALLS edges resolved. The dominant cause is
		// chained-selector call sites the tree-sitter extractor does not
		// yet model (epic solov2-9rc2); an agent that reads {edges:[]}
		// without context would incorrectly conclude the symbol has no
		// callees. We check the seed's kind on the loaded graph rather
		// than re-fetching it.
		reasons := []string{}
		if len(resultEdges) == 0 && len(crossRepoEdges) == 0 && dirOut {
			if seed, ok := g.Node(startID); ok {
				switch seed.Kind {
				case domain.KindFunction, domain.KindMethod:
					reasons = append(reasons, DegradedReasonChainedSelectorsUnresolved)
				}
			}
		}
		return callChainResponse{
			Nodes:           nodesToDTO(resultNodes),
			Edges:           edgesToDTO(resultEdges),
			CrossRepoEdges:  crossRepoEdges,
			IncludedStaging: false,
			DegradedReasons: reasons,
		}, nil
	}
}

// isCallableKind reports whether a node kind represents an actual call
// site source. Containers (package, file, module, chunk) sometimes
// appear as edge sources when the extractor can't pin a call to a
// specific function — filter them out of caller-listings so users see
// real callers (solov2-rkc5).
func isCallableKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindPackage, domain.KindFile, domain.KindModule, domain.KindChunk:
		return false
	default:
		return true
	}
}
