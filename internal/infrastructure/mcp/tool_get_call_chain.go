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

func makeGetCallChainHandler(graph ports.GraphStorage, resolve ResolveFunc, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getCallChainParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.NodeID == "" && p.Symbol == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "missing required params: node_id or symbol"}
		}
		// solov2-ktz0: shim-injected cwd resolves repo_id when omitted.
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

		// solov2-lcz6: accept 'symbol' as an alternative to 'node_id' to give
		// parity with eng_find_symbol. When both are supplied node_id wins —
		// it is the more specific selector. When only symbol is given, look it
		// up via FindNodes; ambiguity (multiple matches) is rejected so the
		// caller has to disambiguate explicitly with node_id.
		if p.NodeID == "" {
			matches, ferr := graph.FindNodes(ctx, p.RepoID, p.Branch, p.Symbol)
			if ferr != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find symbol %q: %v", p.Symbol, ferr)}
			}
			if len(matches) == 0 {
				return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("symbol not found: %s", p.Symbol)}
			}
			if len(matches) > 1 {
				return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("symbol %q is ambiguous (%d matches); pass node_id to disambiguate", p.Symbol, len(matches))}
			}
			p.NodeID = string(matches[0].ID)
		}
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
			if resolve != nil {
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

		return callChainResponse{
			Nodes:           nodesToDTO(resultNodes),
			Edges:           edgesToDTO(resultEdges),
			CrossRepoEdges:  crossRepoEdges,
			IncludedStaging: false,
		}, nil
	}
}
