package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// BlastResponse is the envelope returned by the eng_get_*_blast_radius tools.
type BlastResponse struct {
	Entries         []blastEntryDTO `json:"entries"`
	Truncated       bool            `json:"truncated"`
	IncludedStaging bool            `json:"included_staging"`
	// CrossRepoEdges are synthetic edges from any visited node into another
	// registered repo, resolved via cross_repo_edge_stubs .
	// Omitted when no resolver is wired or no stubs match — same convention
	// as eng_get_call_chain.
	CrossRepoEdges []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	// DegradedReasons / IndexingRepos surface the cold-scan-in-progress
	// window (solov2-izh6.30) so an empty/sparse blast during indexing is
	// distinguishable from a genuinely-isolated symbol. Both omitted when
	// empty so the pre-bead JSON shape is preserved.
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
	IndexingRepos   []string `json:"indexing_repos,omitempty"`
}

// RepoRootFunc returns the absolute path of the working tree for a given
// repoID. It is injected into RegisterBlastTools to keep the MCP layer
// from importing the workspace registry directly.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// BlastToolOption configures optional blast-tool dependencies — primarily the
// cross-repo stub resolver used to expand the BFS frontier into other repos
// . Composition roots without a resolver simply omit it.
type BlastToolOption func(*blastToolConfig)

type blastToolConfig struct {
	resolve        ResolveFunc
	resolveInbound InboundResolveFunc
	scans          ScanTrackerReader
}

// WithBlastScanTracker supplies the daemon's cold-scan tracker so empty
// blast responses can carry an indexing_in_progress hint when a scan is
// in flight (solov2-izh6.30). Nil disables the hint.
func WithBlastScanTracker(t ScanTrackerReader) BlastToolOption {
	return func(c *blastToolConfig) { c.scans = t }
}

// WithBlastResolveFunc supplies a ResolveFunc that the blast handlers use to
// turn each visited node's cross_repo_edge_stubs into CrossRepoEdges in the
// response — parity with eng_get_call_chain's WithResolveFunc.
func WithBlastResolveFunc(fn ResolveFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.resolve = fn }
}

// WithBlastInboundResolveFunc supplies an InboundResolveFunc so blast in
// the callers direction (or DirBoth) also surfaces cross-repo callers in
// OTHER repos targeting the visited nodes. Without it, blast_radius on a
// library symbol cannot see consumers in workspace repos — the
// library-author journey gap closed by solov2-80hh.
func WithBlastInboundResolveFunc(fn InboundResolveFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.resolveInbound = fn }
}

// RegisterBlastTools registers the three blast-radius tools: by-node,
// by-staging, and by-working-tree-diff. svc is required for all three.
//
// repoRoot and changedFiles are required only by eng_get_diff_blast_radius.
// When either is nil the tool is still registered but will return
// InternalError on every call — this keeps the registry uniform across
// composition roots that have not wired the git adapter.
func RegisterBlastTools(r *Registry, svc *blastradius.Service, repoRoot RepoRootFunc, changedFiles blastradius.ChangedFilesFunc, repos application.RepoLister, graph ports.GraphReader, opts ...BlastToolOption) {
	cfg := blastToolConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	r.MustRegister(ToolSpec{
		Name:            "eng_get_blast_radius",
		Description:     DescBlastRadius,
		IncludesStaging: false,
		InputSchema:     blastRadiusInputSchema,
		Handler:         makeBlastRadiusHandler(svc, repos, graph, cfg.resolve, cfg.resolveInbound, cfg.scans),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_dirty_blast_radius",
		Description:     DescDirtyBlastRadius,
		IncludesStaging: true,
		InputSchema:     dirtyBlastRadiusInputSchema,
		Handler:         makeDirtyBlastRadiusHandler(svc, repos, cfg.resolve, cfg.resolveInbound),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_diff_blast_radius",
		Description:     DescDiffBlastRadius,
		IncludesStaging: false,
		InputSchema:     diffBlastRadiusInputSchema,
		Handler:         makeDiffBlastRadiusHandler(svc, repoRoot, changedFiles, repos, cfg.resolve, cfg.resolveInbound),

		CLIExempt: ExemptDeferred,

		ExemptReason: "CLI wrapper deferred (see follow-up tracker referenced in commit history).",
	})
}

type blastRadiusParams struct {
	NodeID          string `json:"node_id"`
	Symbol          string `json:"symbol"`
	RepoID          string `json:"repo_id"`
	Branch          string `json:"branch"`
	MaxDepth        int    `json:"max_depth,omitempty"`
	MaxNodes        int    `json:"max_nodes,omitempty"`
	Direction       string `json:"direction,omitempty"`
	ExpandCrossRepo bool   `json:"expand_cross_repo,omitempty"`
}

func makeBlastRadiusHandler(svc *blastradius.Service, repos application.RepoLister, graph ports.GraphReader, resolve ResolveFunc, resolveInbound InboundResolveFunc, scans ScanTrackerReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p blastRadiusParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-f0zt: fan-out by seed when repo_id is omitted (same contract
		// as `veska blast --help`: "default: fan out across registered repos").
		repoID, branch, nid, rpcErr := resolveSeedOwner(ctx, repos, graph, raw, p.RepoID, p.Branch, p.NodeID, p.Symbol)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID, p.Branch, p.NodeID = repoID, branch, nid
		dir, err := blastradius.ParseDirection(p.Direction)
		if err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		resp, err := svc.Of(ctx, p.RepoID, p.Branch, []string{p.NodeID}, blastradius.Options{
			MaxDepth:  p.MaxDepth,
			MaxNodes:  p.MaxNodes,
			Direction: dir,
		})
		if err != nil {
			if errors.Is(err, blastradius.ErrSeedNotFound) {
				return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node not found in repo=%s branch=%s: %s (pass the full node_id from eng_find_symbol, not the 12-char display prefix)", p.RepoID, p.Branch, p.NodeID)}
			}
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("blast radius: %v", err)}
		}
		crossRepoEdges := mergeCrossRepoEdges(
			resolveCrossRepoFor(ctx, resolve, resp.Entries, p.Branch, p.ExpandCrossRepo),
			resolveCrossRepoInboundFor(ctx, resolveInbound, resp.Entries, p.Branch, dir),
		)
		var reasons, indexing []string
		// solov2-izh6.30: surface the cold-scan window on a sparse blast
		// (entries==[seed-only] or empty + no cross-repo). An ongoing scan
		// may still be populating the target repo's edges.
		if len(resp.Entries) <= 1 && len(crossRepoEdges) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		return BlastResponse{
			Entries:         blastEntriesToDTO(resp.Entries),
			Truncated:       resp.Truncated,
			IncludedStaging: resp.IncludedStaging,
			CrossRepoEdges:  crossRepoEdges,
			DegradedReasons: reasons,
			IndexingRepos:   indexing,
		}, nil
	}
}

// resolveCrossRepoInboundFor mirrors resolveCrossRepoFor but asks the
// inbound resolver for each entry: "which stubs in OTHER repos point at
// this node?" Returns nil when direction is callees-only — inbound
// expansion only makes sense when the user actually wants callers
// . Silent on per-node errors (a stuck remote repo must not
// break the primary blast result).
func resolveCrossRepoInboundFor(ctx context.Context, resolve InboundResolveFunc, entries []blastradius.Entry, branch string, dir blastradius.Direction) []CrossRepoEdge {
	if resolve == nil || len(entries) == 0 {
		return nil
	}
	if dir == blastradius.DirCallees {
		return nil
	}
	var out []CrossRepoEdge
	seen := make(map[string]bool)
	for _, e := range entries {
		resolved, err := resolve(ctx, e.NodeID, branch)
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

// mergeCrossRepoEdges concatenates outbound and inbound edge lists while
// deduping on the (src, dst, kind) triple — the two resolvers can describe
// the same edge from opposite perspectives and the caller should see it
// once.
func mergeCrossRepoEdges(out, in []CrossRepoEdge) []CrossRepoEdge {
	if len(out) == 0 {
		return in
	}
	if len(in) == 0 {
		return out
	}
	seen := make(map[string]bool, len(out)+len(in))
	merged := make([]CrossRepoEdge, 0, len(out)+len(in))
	for _, e := range out {
		key := e.SrcNodeID + "→" + e.DstNodeID + "/" + e.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, e)
	}
	for _, e := range in {
		key := e.SrcNodeID + "→" + e.DstNodeID + "/" + e.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, e)
	}
	return merged
}

// resolveCrossRepoFor walks each entry's cross_repo_edge_stubs via the
// injected resolver and collects the resolved edges. Silent on per-node
// errors (matches call_chain) — cross-repo expansion is advisory and a
// stuck repo must not break the primary blast result. nil resolve is a
// no-op.
func resolveCrossRepoFor(ctx context.Context, resolve ResolveFunc, entries []blastradius.Entry, branch string, expand bool) []CrossRepoEdge {
	if resolve == nil || len(entries) == 0 {
		return nil
	}
	var out []CrossRepoEdge
	seen := make(map[string]bool)
	for _, e := range entries {
		resolved, err := resolve(ctx, e.NodeID, branch, expand)
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

type diffBlastRadiusParams struct {
	RepoID          string `json:"repo_id"`
	Branch          string `json:"branch"`
	MaxDepth        int    `json:"max_depth,omitempty"`
	MaxNodes        int    `json:"max_nodes,omitempty"`
	Direction       string `json:"direction,omitempty"`
	ExpandCrossRepo bool   `json:"expand_cross_repo,omitempty"`
}

func makeDiffBlastRadiusHandler(svc *blastradius.Service, repoRoot RepoRootFunc, changedFiles blastradius.ChangedFilesFunc, repos application.RepoLister, resolve ResolveFunc, resolveInbound InboundResolveFunc) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if repoRoot == nil || changedFiles == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "diff blast radius is not wired (repoRoot or changedFiles missing)",
			}
		}
		var p diffBlastRadiusParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
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
		dir, err := blastradius.ParseDirection(p.Direction)
		if err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		root, err := repoRoot(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
		}
		if root == "" {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
		}
		// Default max_nodes for diff-blast is wider than the by-node default:
		// changes typically span many seeds and a too-tight cap would clip
		// most answers.
		if p.MaxNodes == 0 {
			p.MaxNodes = 500
		}
		resp, err := svc.DiffOf(ctx, p.RepoID, p.Branch, root, changedFiles, blastradius.Options{
			MaxDepth:  p.MaxDepth,
			MaxNodes:  p.MaxNodes,
			Direction: dir,
		})
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("diff blast radius: %v", err)}
		}
		return BlastResponse{
			Entries:         blastEntriesToDTO(resp.Entries),
			Truncated:       resp.Truncated,
			IncludedStaging: resp.IncludedStaging,
			CrossRepoEdges: mergeCrossRepoEdges(
				resolveCrossRepoFor(ctx, resolve, resp.Entries, p.Branch, p.ExpandCrossRepo),
				resolveCrossRepoInboundFor(ctx, resolveInbound, resp.Entries, p.Branch, dir),
			),
		}, nil
	}
}

type dirtyBlastRadiusParams struct {
	RepoID          string `json:"repo_id"`
	Branch          string `json:"branch"`
	MaxDepth        int    `json:"max_depth,omitempty"`
	MaxNodes        int    `json:"max_nodes,omitempty"`
	Direction       string `json:"direction,omitempty"`
	ExpandCrossRepo bool   `json:"expand_cross_repo,omitempty"`
}

func makeDirtyBlastRadiusHandler(svc *blastradius.Service, repos application.RepoLister, resolve ResolveFunc, resolveInbound InboundResolveFunc) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p dirtyBlastRadiusParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
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
		dir, err := blastradius.ParseDirection(p.Direction)
		if err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		resp, err := svc.DirtyOf(ctx, p.RepoID, p.Branch, blastradius.Options{
			MaxDepth:  p.MaxDepth,
			MaxNodes:  p.MaxNodes,
			Direction: dir,
		})
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("dirty blast radius: %v", err)}
		}
		return BlastResponse{
			Entries:         blastEntriesToDTO(resp.Entries),
			Truncated:       resp.Truncated,
			IncludedStaging: resp.IncludedStaging,
			CrossRepoEdges: mergeCrossRepoEdges(
				resolveCrossRepoFor(ctx, resolve, resp.Entries, p.Branch, p.ExpandCrossRepo),
				resolveCrossRepoInboundFor(ctx, resolveInbound, resp.Entries, p.Branch, dir),
			),
		}, nil
	}
}
