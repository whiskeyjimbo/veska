package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// BlastResponse is the envelope returned by the eng_get_*_blast_radius tools.
type BlastResponse struct {
	Entries         []blastradius.Entry `json:"entries"`
	Truncated       bool                `json:"truncated"`
	IncludedStaging bool                `json:"included_staging"`
}

// RepoRootFunc returns the absolute path of the working tree for a given
// repoID. It is injected into RegisterBlastTools to keep the MCP layer
// from importing the workspace registry directly.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// RegisterBlastTools registers the three blast-radius tools: by-node,
// by-staging, and by-working-tree-diff. svc is required for all three.
//
// repoRoot and changedFiles are required only by eng_get_diff_blast_radius.
// When either is nil the tool is still registered but will return
// InternalError on every call — this keeps the registry uniform across
// composition roots that have not wired the git adapter.
func RegisterBlastTools(r *Registry, svc *blastradius.Service, repoRoot RepoRootFunc, changedFiles blastradius.ChangedFilesFunc) {
	r.MustRegister(ToolSpec{
		Name:            "eng_get_blast_radius",
		Description:     "Compute the blast radius (callers/callees/both) of a single node via BFS over the edges table.",
		IncludesStaging: false,
		Handler:         makeBlastRadiusHandler(svc),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_dirty_blast_radius",
		Description:     "Compute the blast radius of all symbols currently in the in-memory staging overlay.",
		IncludesStaging: true,
		Handler:         makeDirtyBlastRadiusHandler(svc),
	})
	r.MustRegister(ToolSpec{
		Name:            "eng_get_diff_blast_radius",
		Description:     "Compute the blast radius for all symbols in files changed in the working-tree diff vs HEAD.",
		IncludesStaging: false,
		Handler:         makeDiffBlastRadiusHandler(svc, repoRoot, changedFiles),
	})
}

type blastRadiusParams struct {
	NodeID    string `json:"node_id"`
	RepoID    string `json:"repo_id"`
	Branch    string `json:"branch"`
	MaxDepth  int    `json:"max_depth,omitempty"`
	MaxNodes  int    `json:"max_nodes,omitempty"`
	Direction string `json:"direction,omitempty"`
}

func makeBlastRadiusHandler(svc *blastradius.Service) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p blastRadiusParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("node_id", p.NodeID, "repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}
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
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("blast radius: %v", err)}
		}
		return BlastResponse{
			Entries:         resp.Entries,
			Truncated:       resp.Truncated,
			IncludedStaging: resp.IncludedStaging,
		}, nil
	}
}

type diffBlastRadiusParams struct {
	RepoID    string `json:"repo_id"`
	Branch    string `json:"branch"`
	MaxDepth  int    `json:"max_depth,omitempty"`
	MaxNodes  int    `json:"max_nodes,omitempty"`
	Direction string `json:"direction,omitempty"`
}

func makeDiffBlastRadiusHandler(svc *blastradius.Service, repoRoot RepoRootFunc, changedFiles blastradius.ChangedFilesFunc) ToolHandler {
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
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
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
			Entries:         resp.Entries,
			Truncated:       resp.Truncated,
			IncludedStaging: resp.IncludedStaging,
		}, nil
	}
}

type dirtyBlastRadiusParams struct {
	RepoID    string `json:"repo_id"`
	Branch    string `json:"branch"`
	MaxDepth  int    `json:"max_depth,omitempty"`
	MaxNodes  int    `json:"max_nodes,omitempty"`
	Direction string `json:"direction,omitempty"`
}

func makeDirtyBlastRadiusHandler(svc *blastradius.Service) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p dirtyBlastRadiusParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
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
			Entries:         resp.Entries,
			Truncated:       resp.Truncated,
			IncludedStaging: resp.IncludedStaging,
		}, nil
	}
}
