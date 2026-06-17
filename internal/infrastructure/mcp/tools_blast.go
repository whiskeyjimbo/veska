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

type BlastResponse struct {
	Entries         []blastEntryDTO `json:"entries"`
	Truncated       bool            `json:"truncated"`
	IncludedStaging bool            `json:"included_staging"`
	// CrossRepoEdges contains resolved synthetic edges targeting external repositories in the workspace.
	CrossRepoEdges []CrossRepoEdge `json:"cross_repo_edges,omitempty"`
	// DegradedReasons and IndexingRepos indicate in-progress cold scans to explain potentially incomplete results.
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
	IndexingRepos   []string `json:"indexing_repos,omitempty"`
	// WakeReconcilingRepos lists repository IDs whose wake reconciliation sweep was active at query time.
	WakeReconcilingRepos []string `json:"wake_reconciling_repos,omitempty"`
}

// RepoRootFunc resolves the workspace root path, allowing decouple from direct imports.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// BlastToolOption configures optional blast-tool extensions such as cross-repository stub resolvers.
type BlastToolOption func(*blastToolConfig)

type blastToolConfig struct {
	resolve             ResolveFunc
	resolveInbound      InboundResolveFunc
	scans               ScanTrackerReader
	reconcile           ReconcileReader
	changedFilesBetween blastradius.ChangedFilesBetweenFunc
}

// WithBlastChangedFilesBetween registers a ranged-diff change lister to support ref ranges on diff-blast queries.
func WithBlastChangedFilesBetween(fn blastradius.ChangedFilesBetweenFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.changedFilesBetween = fn }
}

// WithBlastScanTracker registers the scan tracker to surface in-progress indexing hints on query results.
func WithBlastScanTracker(t ScanTrackerReader) BlastToolOption {
	return func(c *blastToolConfig) { c.scans = t }
}

// WithBlastReconcileTracker registers a reconciler to surface active wake reconciliation hints.
func WithBlastReconcileTracker(t ReconcileReader) BlastToolOption {
	return func(c *blastToolConfig) { c.reconcile = t }
}

// WithBlastResolveFunc registers a callback to resolve outbound cross-repository edges.
func WithBlastResolveFunc(fn ResolveFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.resolve = fn }
}

// WithBlastInboundResolveFunc registers a callback to resolve inbound cross-repository edges from dependent workspace repos.
func WithBlastInboundResolveFunc(fn InboundResolveFunc) BlastToolOption {
	return func(c *blastToolConfig) { c.resolveInbound = fn }
}

// RegisterBlastTools registers blast-radius analysis tools, returning internal errors if required git dependencies are not wired.
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
		// Omitting the repo ID prompts a fan-out search across all registered repositories.
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
		// We surface indexing status for sparse results as the database edges might still be indexing.
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

// resolveCrossRepoInboundFor queries the resolver for external incoming edges, ignoring individual remote lookup errors to prevent system-wide blocking.
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

// mergeCrossRepoEdges combines outbound and inbound cross-repository edges, deduplicating matching pairs.
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

// resolveCrossRepoFor queries the resolver for outbound cross-repository edges, ignoring individual repository errors to maintain availability.
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

// DiffBlastDeps groups git adapter dependencies to avoid excessive parameter lists.
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

// prepareDiffBlast extracts and validates the parameters and repository boundaries required to run a diff blast.
func prepareDiffBlast(ctx context.Context, repos application.RepoLister, deps DiffBlastDeps, raw json.RawMessage) (diffBlastRadiusParams, string, blastradius.Direction, *RPCError) {
	var p diffBlastRadiusParams
	if deps.RepoRoot == nil || deps.ChangedFiles == nil {
		return p, "", "", &RPCError{Code: CodeInternalError, Message: "diff blast radius is not wired (repoRoot or changedFiles missing)"}
	}
	if rpcErr := bindParams(raw, &p); rpcErr != nil {
		return p, "", "", rpcErr
	}
	// Ref parameters are all-or-nothing; passing a single ref is rejected to prevent masking typos.
	if (p.RefA == "") != (p.RefB == "") {
		return p, "", "", &RPCError{Code: CodeInvalidParams, Message: "ref_a and ref_b must be provided together (or both omitted to diff the working tree against HEAD)"}
	}
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
	// We default max_nodes to a larger threshold for diff blasts since changed files naturally seed more expansion paths.
	if p.MaxNodes == 0 {
		p.MaxNodes = 500
	}
	return p, root, dir, nil
}

// diffBlastChangeLister returns the appropriate change function based on whether a working tree or ranged reference diff is requested.
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

// diffBlastError maps git errors to validation or internal errors, checking which references failed to resolve to provide helpful messages.
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
