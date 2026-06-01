package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
)

// ContextPackOption configures RegisterContextPackTool. Today the only
// option is the cross-repo resolver; the variadic shape leaves room for
// future knobs without another signature break.
type ContextPackOption func(*contextPackConfig)

type contextPackConfig struct {
	resolve        ResolveFunc
	resolveInbound InboundResolveFunc
	scans          ScanTrackerReader
}

// WithContextPackScanTracker supplies the daemon's cold-scan tracker so
// sparse context packs can carry an indexing_in_progress hint when a
// scan is in flight at query time (solov2-izh6.30). Nil disables the hint.
func WithContextPackScanTracker(t ScanTrackerReader) ContextPackOption {
	return func(c *contextPackConfig) { c.scans = t }
}

// WithContextPackResolveFunc supplies a ResolveFunc so the handler can
// turn each pack node's cross_repo_edge_stubs into CrossRepoEdges on the
// response — parity with eng_get_call_chain and eng_get_blast_radius
// . Without it the response carries no cross_repo_edges and
// agents reading the pack alone cannot see consumers in other repos.
func WithContextPackResolveFunc(fn ResolveFunc) ContextPackOption {
	return func(c *contextPackConfig) { c.resolve = fn }
}

// WithContextPackInboundResolveFunc supplies an InboundResolveFunc so the
// context-pack response also surfaces cross-repo CALLERS of each node in
// the pack. The pack's blast walks DirBoth, so the typical user question
// "show me the surrounding neighbourhood" needs both directions of
// cross-repo edges to be honest .
func WithContextPackInboundResolveFunc(fn InboundResolveFunc) ContextPackOption {
	return func(c *contextPackConfig) { c.resolveInbound = fn }
}

// contextPackResponse embeds the application Pack so the existing JSON
// shape is preserved bit-for-bit, then layers CrossRepoEdges on top —
// keeping the application layer free of an mcp-specific edge type.
type contextPackResponse struct {
	contextpack.Pack
	CrossRepoEdges []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	// DegradedReasons / IndexingRepos surface the cold-scan-in-progress
	// window (solov2-izh6.30) so a sparse pack during indexing is
	// distinguishable from a genuinely isolated symbol.
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
	IndexingRepos   []string `json:"indexing_repos,omitempty"`
}

// RegisterContextPackTool registers eng_get_context_pack. asm and repoRoot
// are required; when either is nil the tool is still registered but
// returns InternalError on every call, keeping the registry uniform
// across composition roots that have not wired the context-pack service.
func RegisterContextPackTool(r *Registry, asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister, opts ...ContextPackOption) {
	var cfg contextPackConfig
	for _, o := range opts {
		o(&cfg)
	}
	r.MustRegister(ToolSpec{
		Name:        "eng_get_context_pack",
		Description: DescContextPack + " Pass exactly one of node_id, symbol, or task_id as the anchor.",
		InputSchema: contextPackInputSchema,
		Handler:     makeContextPackHandler(asm, repoRoot, repos, cfg.resolve, cfg.resolveInbound, cfg.scans),
	})
}

type contextPackParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	NodeID string `json:"node_id,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

func makeContextPackHandler(asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister, resolve ResolveFunc, resolveInbound InboundResolveFunc, scans ScanTrackerReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if asm == nil || repoRoot == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "context pack is not wired (assembler or repoRoot missing)",
			}
		}
		var p contextPackParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// Exactly one of node_id / symbol / task_id is required .
		anchorCount := 0
		if p.NodeID != "" {
			anchorCount++
		}
		if p.Symbol != "" {
			anchorCount++
		}
		if p.TaskID != "" {
			anchorCount++
		}
		if anchorCount != 1 {
			return nil, &RPCError{
				Code:    CodeInvalidParams,
				Message: "exactly one of node_id, symbol or task_id is required",
			}
		}
		// solov2-z5cu: parity with eng_find_symbol. When the caller asks
		// for a symbol-anchored pack and repo_id is omitted, probe every
		// registered repo and auto-pick if exactly one contains the
		// symbol. Multi-repo workspaces would otherwise dead-end on
		// "repo_id is required" even when the symbol is unambiguous.
		if p.Symbol != "" && p.RepoID == "" {
			chosen, rpcErr := pickRepoForSymbol(ctx, asm, repoRoot, repos, raw, p.Symbol, p.Branch)
			if rpcErr != nil {
				return nil, rpcErr
			}
			if chosen != "" {
				p.RepoID = chosen
			}
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
		root, err := repoRoot(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
		}
		if root == "" {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
		}

		var pack contextpack.Pack
		switch {
		case p.NodeID != "":
			pack, err = asm.ForNode(ctx, p.RepoID, p.Branch, root, p.NodeID)
		case p.Symbol != "":
			pack, err = asm.ForSymbol(ctx, p.RepoID, p.Branch, root, p.Symbol)
		default:
			pack, err = asm.ForTask(ctx, p.RepoID, p.Branch, root, p.TaskID)
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("context pack: %v", err)}
		}
		// solov2-7xrw: resolve cross_repo_edge_stubs for each node in the
		// pack so an agent (or the CLI wrapper) can see consumers/callees
		// in other registered repos in the same response. Without this
		// step a junior asking 'what does Run touch?' on a multi-repo
		// workspace gets only same-repo nodes even when the parser
		// captured the cross-repo edge.
		crossRepo := mergeCrossRepoEdges(
			resolveCrossRepoForNodes(ctx, resolve, pack.Nodes, p.Branch),
			resolveCrossRepoInboundForNodes(ctx, resolveInbound, pack.Nodes, p.Branch),
		)
		var reasons, indexing []string
		// solov2-izh6.30: a pack with only the seed node and no cross-repo
		// edges, while a scan is in flight, is the indexing-window case.
		// Surface so the caller can retry instead of treating the pack as
		// the final shape.
		if len(pack.Nodes) <= 1 && len(crossRepo) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		return contextPackResponse{
			Pack:            pack,
			CrossRepoEdges:  crossRepo,
			DegradedReasons: reasons,
			IndexingRepos:   indexing,
		}, nil
	}
}

// resolveCrossRepoInboundForNodes is the contextpack analogue of
// resolveCrossRepoInboundFor: for each node in the pack, ask "who in
// OTHER repos calls this?" and return the inbound edges .
// The pack always walks DirBoth, so unlike the blast handler there's no
// direction gating here — both perspectives are always relevant.
func resolveCrossRepoInboundForNodes(ctx context.Context, resolve InboundResolveFunc, nodes []contextpack.NodeInfo, branch string) []CrossRepoEdge {
	if resolve == nil || len(nodes) == 0 {
		return nil
	}
	var out []CrossRepoEdge
	seen := make(map[string]bool)
	for _, n := range nodes {
		resolved, err := resolve(ctx, n.NodeID, branch)
		if err != nil {
			continue
		}
		for _, re := range resolved {
			key := re.SrcNodeID + "→" + re.DstNodeID + "/" + re.Kind
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, CrossRepoEdge{
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
	return out
}

// pickRepoForSymbol probes every (repoID, branch) target a fanout would
// produce and reports the unique repo that contains the symbol, or "" when
// auto-resolution should not apply. Returns an InvalidParams error listing
// the candidates when more than one repo contains the symbol — the caller
// must pass --repo to disambiguate .
//
// Errors from any single asm.ForSymbol probe are swallowed so a stuck repo
// can't poison the auto-resolution for the others.
func pickRepoForSymbol(ctx context.Context, asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister, raw json.RawMessage, symbol, callerBranch string) (string, *RPCError) {
	if asm == nil || repoRoot == nil {
		return "", nil
	}
	targets, fanout, rpcErr := resolveRepoFanoutFromParams(ctx, repos, raw, "", callerBranch)
	if rpcErr != nil {
		return "", nil // let the normal resolver re-emit a coherent error
	}
	if !fanout {
		// Singleton or cwd already pinned a single repo; let the normal
		// path handle it.
		return "", nil
	}
	var hits []repoBranch
	for _, t := range targets {
		root, err := repoRoot(ctx, t.RepoID)
		if err != nil || root == "" {
			continue
		}
		pack, perr := asm.ForSymbol(ctx, t.RepoID, t.Branch, root, symbol)
		if perr != nil {
			continue
		}
		if len(pack.Nodes) > 0 {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		// No repo contains the symbol — let the normal path emit the
		// existing "repo_id is required" error.
		return "", nil
	case 1:
		return hits[0].RepoID, nil
	default:
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = ShortRepoID(h.RepoID)
		}
		return "", &RPCError{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("symbol %q matches in %d repos (%s); pass repo_id to disambiguate", symbol, len(hits), strings.Join(ids, ",")),
		}
	}
}

// resolveCrossRepoForNodes mirrors resolveCrossRepoFor in tools_blast.go
// but takes a contextpack.NodeInfo slice instead of blastradius.Entry.
// Silent on per-node errors — a stuck repo must not break the primary
// pack. nil resolve is a no-op.
func resolveCrossRepoForNodes(ctx context.Context, resolve ResolveFunc, nodes []contextpack.NodeInfo, branch string) []CrossRepoEdge {
	if resolve == nil || len(nodes) == 0 {
		return nil
	}
	var out []CrossRepoEdge
	seen := make(map[string]bool)
	for _, n := range nodes {
		resolved, err := resolve(ctx, n.NodeID, branch, false)
		if err != nil {
			continue
		}
		for _, re := range resolved {
			key := re.SrcNodeID + "→" + re.DstNodeID + "/" + re.Kind
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, CrossRepoEdge{
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
	return out
}
