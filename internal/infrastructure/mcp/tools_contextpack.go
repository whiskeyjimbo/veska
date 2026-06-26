// SPDX-License-Identifier: AGPL-3.0-only

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
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// ContextPackOption configures RegisterContextPackTool, using a variadic shape to support future configuration parameters without breaking the function signature.
type ContextPackOption func(*contextPackConfig)

type contextPackConfig struct {
	resolve        ResolveFunc
	resolveInbound InboundResolveFunc
	scans          ScanTrackerReader
	reconcile      ReconcileReader
}

// WithContextPackScanTracker supplies the cold-scan tracker so that sparse context packs can include an indexing-in-progress hint when a scan is currently active.
func WithContextPackScanTracker(t ScanTrackerReader) ContextPackOption {
	return func(c *contextPackConfig) { c.scans = t }
}

// WithContextPackReconcileTracker supplies the wake reconciler so that packs include a reconciling hint when a repository sweep is currently active.
func WithContextPackReconcileTracker(t ReconcileReader) ContextPackOption {
	return func(c *contextPackConfig) { c.reconcile = t }
}

// WithContextPackResolveFunc supplies a ResolveFunc to translate cross-repo edge stubs into full CrossRepoEdges, allowing callers to view consumer relationships across repository boundaries.
func WithContextPackResolveFunc(fn ResolveFunc) ContextPackOption {
	return func(c *contextPackConfig) { c.resolve = fn }
}

// WithContextPackInboundResolveFunc surfaces cross-repo callers of each node in the pack to ensure both directions of cross-repo relationships are represented.
func WithContextPackInboundResolveFunc(fn InboundResolveFunc) ContextPackOption {
	return func(c *contextPackConfig) { c.resolveInbound = fn }
}

// contextPackResponse embeds the core application Pack to preserve the public JSON schema while adding cross-repo metadata without introducing MCP details to the core application package.
type contextPackResponse struct {
	contextpack.Pack
	CrossRepoEdges []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	// DegradedReasons and IndexingRepos indicate whether a sparse result is due to ongoing background indexing.
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
	IndexingRepos   []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos lists repositories currently undergoing an active wake sweep.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// RegisterContextPackTool registers eng_get_context_pack, allowing tool registration to proceed even if dependencies are missing to maintain registry uniformity across all environments.
func RegisterContextPackTool(r *Registry, asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister, opts ...ContextPackOption) {
	var cfg contextPackConfig
	for _, o := range opts {
		o(&cfg)
	}
	r.MustRegister(ToolSpec{
		Name:        "eng_get_context_pack",
		Description: DescContextPack + " Pass exactly one of node_id, symbol, or task_id as the anchor.",
		Tier:        Tier1,
		InputSchema: contextPackInputSchema,
		Handler:     makeContextPackHandler(asm, repoRoot, repos, cfg.resolve, cfg.resolveInbound, cfg.scans, cfg.reconcile),
	})
}

type contextPackParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	NodeID string `json:"node_id,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	// Scope bounds the neighborhood width: "focused" returns the seed plus its
	// direct callees only (cheapest), "full" (default) returns both directions
	// at default depth. See contextpack.Scope for the token-budget rationale.
	Scope string `json:"scope,omitempty"`
}

func makeContextPackHandler(asm *contextpack.Assembler, repoRoot RepoRootFunc, repos application.RepoLister, resolve ResolveFunc, resolveInbound InboundResolveFunc, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
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
		// Exactly one of node_id / symbol / task_id is required.
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
		scope, err := contextpack.ParseScope(p.Scope)
		if err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		packOpts := contextpack.PackOptions{Scope: scope}
		// If repo_id is omitted for a symbol, all registered repositories are queried to automatically resolve the repository when the symbol is unambiguous.
		if p.Symbol != "" && p.RepoID == "" {
			chosen, rpcErr := pickRepoForSymbol(ctx, asm, repoRoot, repos, raw, p.Symbol, p.Branch)
			if rpcErr != nil {
				return nil, rpcErr
			}
			if chosen != "" {
				p.RepoID = chosen
			}
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
			pack, err = asm.ForNode(ctx, p.RepoID, p.Branch, root, p.NodeID, packOpts)
		case p.Symbol != "":
			pack, err = asm.ForSymbol(ctx, p.RepoID, p.Branch, root, p.Symbol, packOpts)
		default:
			pack, err = asm.ForTask(ctx, p.RepoID, p.Branch, root, p.TaskID, packOpts)
		}
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("context pack: %v", err)}
		}
		// Cross-repo edge stubs are fully resolved here to ensure callers receive cross-repo dependencies in a single response payload.
		crossRepo := mergeCrossRepoEdges(
			resolveCrossRepoForNodes(ctx, resolve, pack.Nodes, p.Branch),
			resolveCrossRepoInboundForNodes(ctx, resolveInbound, pack.Nodes, p.Branch),
		)
		var reasons, indexing []string
		// Packs containing only the seed node during an active scan trigger an indexing degraded state to signal that results may be incomplete.
		if len(pack.Nodes) <= 1 && len(crossRepo) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		reconciling := reconcilingForRepos(reconcile, []string{p.RepoID})
		if len(reconciling) > 0 {
			reasons = append(reasons, protocol.DegradedReasonWakeReconciling)
		}
		// A shallow clone has one commit, so the pack's per-file history is
		// truncated and not authoritative.
		if shallow, serr := gitinfra.IsShallow(ctx, root); serr == nil && shallow {
			reasons = AppendDegradedReason(reasons, protocol.DegradedReasonShallowClone)
		}
		return contextPackResponse{
			Pack:                 pack,
			CrossRepoEdges:       crossRepo,
			DegradedReasons:      reasons,
			IndexingRepos:        indexing,
			WakeReconcilingRepos: reconciling,
		}, nil
	}
}

// resolveCrossRepoInboundForNodes finds inbound caller relationships from other repositories for all nodes in the pack.
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

// pickRepoForSymbol identifies the unique repository containing the symbol when repository auto-resolution applies. Individual repository query errors are ignored to prevent a misconfigured repository from breaking lookup for others.
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
		// Disambiguation only needs to know whether the symbol resolves in
		// this repo, so the default scope is fine here.
		pack, perr := asm.ForSymbol(ctx, t.RepoID, t.Branch, root, symbol, contextpack.PackOptions{})
		if perr != nil {
			continue
		}
		if len(pack.Nodes) > 0 {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		// No repo contains the symbol - let the normal path emit the
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

// resolveCrossRepoForNodes resolves outbound cross-repo edges for all nodes in the pack. Individual node errors are swallowed to keep the primary pack response functional.
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
