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
// eng_get_node
// ---------------------------------------------------------------------------

type getNodeParams struct {
	NodeID string `json:"node_id"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

func makeGetNodeHandler(graph ports.GraphStorage, staging *application.StagingArea) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getNodeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.NodeID == "" || p.RepoID == "" || p.Branch == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "node_id, repo_id, and branch are required"}
		}

		node, err := graph.GetNode(ctx, p.RepoID, p.Branch, domain.NodeID(p.NodeID))
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("graph lookup failed: %v", err)}
		}

		// Check staging overlay: scan all staged files for this node ID.
		includedStaging := false
		stagedFiles := staging.StagedFiles(p.RepoID, p.Branch)
		for _, fp := range stagedFiles {
			stagedNodes, ok := staging.GetStagedNodes(p.RepoID, p.Branch, fp)
			if !ok {
				continue
			}
			for _, sn := range stagedNodes {
				if sn.ID == domain.NodeID(p.NodeID) {
					node = sn
					includedStaging = true
					break
				}
			}
			if includedStaging {
				break
			}
		}

		if node == nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("node not found: %s", p.NodeID)}
		}

		return GraphResponse{
			Nodes:           []*domain.Node{node},
			IncludedStaging: includedStaging,
		}, nil
	}
}
