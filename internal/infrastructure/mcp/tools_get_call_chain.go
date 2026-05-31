package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// chainedSelectorCallRe matches a call expression whose function is a
// selector chain of at least two dots: `a.b.c(`, `pkg.Type.Method(`,
// `obj.field.M(` etc. The legacy parser  does not model
// these as edges, so an empty resolved-edge set on a seed whose body
// contains this shape is a parser limitation rather than an index gap.
var chainedSelectorCallRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*){2,}\s*\(`)

// seedBodyContainsChainedSelector reports whether the seed body contains
// a chained selector call. Empty body (snippet unavailable, file truly
// empty) is treated conservatively as "yes" so the legacy
// chained_selectors_unresolved hint still fires — the discriminator only
// narrows when we have evidence the body has no chained selectors.
func seedBodyContainsChainedSelector(body string) bool {
	if body == "" {
		return true
	}
	return chainedSelectorCallRe.MatchString(body)
}

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

func makeGetCallChainHandler(graph ports.GraphReader, resolve ResolveFunc, resolveInbound InboundResolveFunc, repos application.RepoLister, scans ScanTrackerReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getCallChainParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		// Param validation that doesn't require seed resolution runs
		// first so cheap "bad request" errors aren't masked by the more
		// expensive resolve+expand round trips (solov2-izh6.1 made
		// resolveSeedOwner expand node_id prefixes, which can now hit
		// NotFound before depth-validation would have fired).
		depth := p.Depth
		if depth <= 0 {
			depth = 3 // default
		}
		if depth > maxCallChainDepth {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("depth %d exceeds maximum of %d", depth, maxCallChainDepth)}
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
							SrcLine:   re.SrcLine,
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

		// solov2-jojv / solov2-izh6.22: emit a degraded_reasons hint
		// when the seed is a callable but no CALLS edges resolved.
		// jojv landed a single chained_selectors_unresolved catch-all;
		// izh6.22 splits it: only emit that reason when the seed's body
		// actually contains a chained selector call site (a.b.c(...))
		// the parser doesn't model (epic solov2-9rc2). Otherwise the
		// dominant cause is external/stdlib callees outside the graph,
		// so emit external_callees_only instead — actionable for an
		// agent ("not a parser bug, just index boundary").
		reasons := []string{}
		var indexing []string
		if len(resultEdges) == 0 && len(crossRepoEdges) == 0 && dirOut {
			if seed, ok := g.Node(startID); ok {
				switch seed.Kind {
				case domain.KindFunction, domain.KindMethod:
					// snippet fetch is best-effort — on error we keep the
					// conservative legacy reason rather than swallow the
					// failure into a different signal.
					body, snipErr := graph.GetNodeSnippet(ctx, p.RepoID, p.Branch, startID)
					if snipErr != nil || seedBodyContainsChainedSelector(body) {
						reasons = append(reasons, DegradedReasonChainedSelectorsUnresolved)
					} else {
						reasons = append(reasons, DegradedReasonExternalCalleesOnly)
					}
				}
			}
			// solov2-izh6.30: a fully empty chain during an active cold
			// scan is the indexing-window case; surface it alongside (or
			// instead of) the seed-based reasons so callers can retry.
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		return callChainResponse{
			Nodes:           nodesToDTO(resultNodes),
			Edges:           edgesToDTO(resultEdges),
			CrossRepoEdges:  crossRepoEdges,
			IndexingRepos:   indexing,
			IncludedStaging: false,
			DegradedReasons: reasons,
		}, nil
	}
}

// isCallableKind reports whether a node kind represents an actual call
// site source. Containers (package, file, module, chunk) sometimes
// appear as edge sources when the extractor can't pin a call to a
// specific function — filter them out of caller-listings so users see
// real callers .
func isCallableKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindPackage, domain.KindFile, domain.KindModule, domain.KindChunk:
		return false
	default:
		return true
	}
}
