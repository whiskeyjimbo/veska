package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ContextPackOption configures RegisterContextPackTool. Today the only
// option is the cross-repo resolver; the variadic shape leaves room for
// future knobs without another signature break.
type ContextPackOption func(*contextPackConfig)

type contextPackConfig struct {
	resolve        ResolveFunc
	resolveInbound InboundResolveFunc
}

// WithContextPackResolveFunc supplies a ResolveFunc so the handler can
// turn each pack node's cross_repo_edge_stubs into CrossRepoEdges on the
// response — parity with eng_get_call_chain and eng_get_blast_radius
// (solov2-7xrw). Without it the response carries no cross_repo_edges and
// agents reading the pack alone cannot see consumers in other repos.
func WithContextPackResolveFunc(fn ResolveFunc) ContextPackOption {
	return func(c *contextPackConfig) { c.resolve = fn }
}

// WithContextPackInboundResolveFunc supplies an InboundResolveFunc so the
// context-pack response also surfaces cross-repo CALLERS of each node in
// the pack. The pack's blast walks DirBoth, so the typical user question
// "show me the surrounding neighbourhood" needs both directions of
// cross-repo edges to be honest (solov2-80hh).
func WithContextPackInboundResolveFunc(fn InboundResolveFunc) ContextPackOption {
	return func(c *contextPackConfig) { c.resolveInbound = fn }
}

// contextPackResponse embeds the application Pack so the existing JSON
// shape is preserved bit-for-bit, then layers CrossRepoEdges on top —
// keeping the application layer free of an mcp-specific edge type.
type contextPackResponse struct {
	contextpack.Pack
	CrossRepoEdges []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
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
		Description: "Bundle a symbol's neighbourhood (callers, callees, adjacent tests, recent commits, open findings, active task) into one token-bounded JSON payload. Use at the START of a non-trivial change so you don't have to assemble surrounding context piecewise with multiple tool calls. Pass exactly one of node_id, symbol, or task_id as the anchor. Surfaces cross_repo_edges in both directions, so cross-repo callers/callees show up in the same response (solov2-7xrw, solov2-80hh).",
		InputSchema: contextPackInputSchema,
		Handler:     makeContextPackHandler(asm, repoRoot, repos, cfg.resolve, cfg.resolveInbound),
	})
}

type contextPackParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	NodeID string `json:"node_id,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

func makeContextPackHandler(asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister, resolve ResolveFunc, resolveInbound InboundResolveFunc) ToolHandler {
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
		// Exactly one of node_id / symbol / task_id is required (solov2-z81b).
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
		return contextPackResponse{
			Pack: pack,
			CrossRepoEdges: mergeCrossRepoEdges(
				resolveCrossRepoForNodes(ctx, resolve, pack.Nodes, p.Branch),
				resolveCrossRepoInboundForNodes(ctx, resolveInbound, pack.Nodes, p.Branch),
			),
		}, nil
	}
}

// resolveCrossRepoInboundForNodes is the contextpack analogue of
// resolveCrossRepoInboundFor: for each node in the pack, ask "who in
// OTHER repos calls this?" and return the inbound edges (solov2-80hh).
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
			})
		}
	}
	return out
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
			})
		}
	}
	return out
}
