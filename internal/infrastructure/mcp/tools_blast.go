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

// RegisterBlastTools registers eng_get_blast_radius and
// eng_get_dirty_blast_radius. svc is required for both.
func RegisterBlastTools(r *Registry, svc *blastradius.Service) {
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
