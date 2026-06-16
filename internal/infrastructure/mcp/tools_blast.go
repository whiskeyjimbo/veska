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
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// BlastResponse is the envelope returned by the eng_get_*_blast_radius tools.
type BlastResponse struct {
	Entries         []blastEntryDTO `json:"entries"`
	Truncated       bool            `json:"truncated"`
	IncludedStaging bool            `json:"included_staging"`
	// CrossRepoEdges are synthetic edges from any visited node into another
	// registered repo, resolved via cross_repo_edge_stubs.
	// Omitted when no resolver is wired or no stubs match — same convention
	// as eng_get_call_chain.
	CrossRepoEdges []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	// DegradedReasons / IndexingRepos surface the cold-scan-in-progress
	// window so an empty/sparse blast during indexing is
	// distinguishable from a genuinely-isolated symbol. Both omitted when
	// empty so the pre-bead JSON shape is preserved.
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
	IndexingRepos   []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos lists the seed's repo when its wake reconcile sweep
	// was in flight at query time. Fires on empty AND
	// non-empty results. Omitted when empty.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// RepoRootFunc returns the absolute path of the working tree for a given
// repoID. It is injected into RegisterBlastTools to keep the MCP layer
// from importing the workspace registry directly.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// BlastToolOption configures optional blast-tool dependencies — primarily the
// cross-repo stub resolver used to expand the BFS frontier into other repos
// Composition roots without a resolver simply omit it.
type BlastToolOption func(*blastToolConfig)

type blastToolConfig struct {
	resolve             ResolveFunc
	resolveInbound      InboundResolveFunc
	scans               ScanTrackerReader
	reconcile           ReconcileReader
	changedFilesBetween blastradius.ChangedFilesBetweenFunc
}

// WithBlastChangedFilesBetween supplies the ranged-diff change-set lister so
// eng_get_diff_blast_radius accepts ref_a/ref_b. Nil (the
// default) leaves the tool working-tree-only: a ranged request then returns
// InternalError, while the bare working-tree diff is unaffected.
func WithBlastChangedFilesBetween(fn blastradius.ChangedFilesBetweenFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.changedFilesBetween = fn }
}

// WithBlastScanTracker supplies the daemon's cold-scan tracker so empty
// blast responses can carry an indexing_in_progress hint when a scan is
// in flight. Nil disables the hint.
func WithBlastScanTracker(t ScanTrackerReader) BlastToolOption {
	return func(c *blastToolConfig) { c.scans = t }
}

// WithBlastReconcileTracker supplies the wake reconciler so a blast on a node
// whose repo is mid-sweep carries a wake_reconciling hint.
// Nil disables the hint.
func WithBlastReconcileTracker(t ReconcileReader) BlastToolOption {
	return func(c *blastToolConfig) { c.reconcile = t }
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
// library-author journey gap closed by.
func WithBlastInboundResolveFunc(fn InboundResolveFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.resolveInbound = fn }
}

// RegisterBlastTools registers the three blast-radius tools: by-node,
// by-staging, and by-working-tree-diff. svc is required for all three.
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
		Handler:         makeBlastRadiusHandler(svc, repos, graph, cfg.resolve, cfg.resolveInbound, cfg.scans, cfg.reconcile),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_dirty_blast_radius",
		Description:     DescDirtyBlastRadius,
		IncludesStaging: true,
		InputSchema:     dirtyBlastRadiusInputSchema,
		Handler:         makeDirtyBlastRadiusHandler(svc, repos, cfg.resolve, cfg.resolveInbound),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_diff_blast_radius",
		Description:     DescDiffBlastRadius,
		IncludesStaging: false,
		InputSchema:     diffBlastRadiusInputSchema,
		Handler: makeDiffBlastRadiusHandler(svc, DiffBlastDeps{
			RepoRoot:            repoRoot,
			ChangedFiles:        changedFiles,
			ChangedFilesBetween: cfg.changedFilesBetween,
		}, repos, cfg.resolve, cfg.resolveInbound),
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

func makeBlastRadiusHandler(svc *blastradius.Service, repos application.RepoLister, graph ports.GraphReader, resolve ResolveFunc, resolveInbound InboundResolveFunc, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p blastRadiusParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// fan-out by seed when repo_id is omitted (same contract
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
		// surface the cold-scan window on a sparse blast
		// (entries==[seed-only] or empty + no cross-repo). An ongoing scan
		// may still be populating the target repo's edges.
		if len(resp.Entries) <= 1 && len(crossRepoEdges) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		reconciling := reconcilingForRepos(reconcile, []string{p.RepoID})
		if len(reconciling) > 0 {
			reasons = append(reasons, protocol.DegradedReasonWakeReconciling)
		}
		return BlastResponse{
			Entries:              blastEntriesToDTO(resp.Entries),
			Truncated:            resp.Truncated,
			IncludedStaging:      resp.IncludedStaging,
			CrossRepoEdges:       crossRepoEdges,
			DegradedReasons:      reasons,
			IndexingRepos:        indexing,
			WakeReconcilingRepos: reconciling,
		}, nil
	}
}

// resolveCrossRepoInboundFor mirrors resolveCrossRepoFor but asks the
// inbound resolver for each entry: "which stubs in OTHER repos point at
// this node?" Returns nil when direction is callees-only — inbound
// expansion only makes sense when the user actually wants callers
// Silent on per-node errors (a stuck remote repo must not
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
	RefA            string `json:"ref_a,omitempty"`
	RefB            string `json:"ref_b,omitempty"`
	MaxDepth        int    `json:"max_depth,omitempty"`
	MaxNodes        int    `json:"max_nodes,omitempty"`
	Direction       string `json:"direction,omitempty"`
	ExpandCrossRepo bool   `json:"expand_cross_repo,omitempty"`
}

// DiffBlastDeps bundles the git adapters eng_get_diff_blast_radius needs:
// the repo-root resolver, the working-tree change-set lister, and the
// optional ranged (ref_a.ref_b) lister. Grouping them keeps the handler
// within the argument budget and collapses the "is diff blast wired" gate to
// one place. ChangedFilesBetween may be nil (working-tree-only deployments).
type DiffBlastDeps struct {
	RepoRoot            RepoRootFunc
	ChangedFiles        blastradius.ChangedFilesFunc
	ChangedFilesBetween blastradius.ChangedFilesBetweenFunc
}

func makeDiffBlastRadiusHandler(svc *blastradius.Service, deps DiffBlastDeps, repos application.RepoLister, resolve ResolveFunc, resolveInbound InboundResolveFunc) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		p, root, dir, rpcErr := prepareDiffBlast(ctx, repos, deps, raw)
		if rpcErr != nil {
			return nil, rpcErr
		}
		listChanged, rpcErr := diffBlastChangeLister(deps, p)
		if rpcErr != nil {
			return nil, rpcErr
		}
		resp, err := svc.DiffOf(ctx, p.RepoID, p.Branch, root, listChanged, blastradius.Options{
			MaxDepth:  p.MaxDepth,
			MaxNodes:  p.MaxNodes,
			Direction: dir,
		})
		if err != nil {
			return nil, diffBlastError(ctx, root, p.RefA, p.RefB, err)
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

// prepareDiffBlast validates and resolves the shared diff-blast preamble:
// the wiring gate, param binding, the all-or-nothing ref_a/ref_b rule, repo /
// branch / direction resolution, the repo root, and the wider default
// max_nodes. It returns the resolved params plus the repo root and direction
// so the handler body stays focused on running the blast.
func prepareDiffBlast(ctx context.Context, repos application.RepoLister, deps DiffBlastDeps, raw json.RawMessage) (diffBlastRadiusParams, string, blastradius.Direction, *RPCError) {
	var p diffBlastRadiusParams
	if deps.RepoRoot == nil || deps.ChangedFiles == nil {
		return p, "", "", &RPCError{Code: CodeInternalError, Message: "diff blast radius is not wired (repoRoot or changedFiles missing)"}
	}
	if rpcErr := bindParams(raw, &p); rpcErr != nil {
		return p, "", "", rpcErr
	}
	// ref_a/ref_b are all-or-nothing: both set selects a ranged diff, both
	// omitted falls back to the working-tree-vs-HEAD default. A lone ref is a
	// caller error — no sensible "the other side is HEAD" default exists that
	// wouldn't silently mask a typo.
	if (p.RefA == "") != (p.RefB == "") {
		return p, "", "", &RPCError{Code: CodeInvalidParams, Message: "ref_a and ref_b must be provided together (or both omitted to diff the working tree against HEAD)"}
	}
	// shim-injected cwd resolves repo_id when omitted.
	repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
	if rpcErr != nil {
		return p, "", "", rpcErr
	}
	p.RepoID = repoID
	br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch)
	if rpcErr != nil {
		return p, "", "", rpcErr
	}
	p.Branch = br
	dir, err := blastradius.ParseDirection(p.Direction)
	if err != nil {
		return p, "", "", &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	root, err := deps.RepoRoot(ctx, p.RepoID)
	if err != nil {
		return p, "", "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
	}
	if root == "" {
		return p, "", "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
	}
	// Default max_nodes for diff-blast is wider than the by-node default:
	// changes typically span many seeds and a too-tight cap would clip most
	// answers.
	if p.MaxNodes == 0 {
		p.MaxNodes = 500
	}
	return p, root, dir, nil
}

// diffBlastChangeLister picks the change-set lister for a diff-blast request:
// the working-tree ChangedFiles by default, or a closure capturing ref_a/ref_b
// when both are supplied. The closure satisfies the working-tree
// ChangedFilesFunc signature so DiffOf stays agnostic about how the change set
// was derived. A ranged request with no ranged lister wired is an
// InternalError.
func diffBlastChangeLister(deps DiffBlastDeps, p diffBlastRadiusParams) (blastradius.ChangedFilesFunc, *RPCError) {
	if p.RefA == "" {
		return deps.ChangedFiles, nil
	}
	if deps.ChangedFilesBetween == nil {
		return nil, &RPCError{Code: CodeInternalError, Message: "ranged diff blast radius is not wired (changedFilesBetween missing)"}
	}
	return func(ctx context.Context, repoRoot string) ([]string, error) {
		return deps.ChangedFilesBetween(ctx, repoRoot, p.RefA, p.RefB)
	}, nil
}

// diffBlastError maps a DiffOf failure onto an RPCError. An unresolvable ref
// (typo, unfetched commit) is a caller problem → InvalidParams naming the
// offending side, rather than leaking raw git stderr as an InternalError.
// refA/refB are empty for the working-tree path, where ErrUnknownRevision
// cannot arise, so the ranged-only messaging is safe.
func diffBlastError(ctx context.Context, root, refA, refB string, err error) *RPCError {
	if !errors.Is(err, gitinfra.ErrUnknownRevision) {
		return &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("diff blast radius: %v", err)}
	}
	aOK := gitinfra.ResolvesRef(ctx, root, refA)
	bOK := gitinfra.ResolvesRef(ctx, root, refB)
	var msg string
	switch {
	case !aOK && !bOK:
		msg = fmt.Sprintf("neither ref_a=%q nor ref_b=%q resolves in this repository — check for typos and verify with `git rev-parse <ref>`", refA, refB)
	case !aOK:
		msg = fmt.Sprintf("ref_a=%q does not resolve in this repository — verify with `git rev-parse %s`", refA, refA)
	case !bOK:
		msg = fmt.Sprintf("ref_b=%q does not resolve in this repository — verify with `git rev-parse %s`", refB, refB)
	default:
		msg = fmt.Sprintf("git diff %s..%s failed despite both refs resolving — try `git diff %s %s` in the repo for details", refA, refB, refA, refB)
	}
	return &RPCError{Code: CodeInvalidParams, Message: msg}
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
		// shim-injected cwd resolves repo_id when omitted.
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
