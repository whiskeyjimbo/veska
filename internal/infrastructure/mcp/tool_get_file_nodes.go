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
// eng_get_file_nodes
// ---------------------------------------------------------------------------

type getFileNodesParams struct {
	FilePath string `json:"file_path"`
	RepoID   string `json:"repo_id"`
	Branch   string `json:"branch"`
}

// makeGetFileNodesHandler returns every node for (repo, branch, file_path).
// Staging overlay takes precedence — if the file is in staging the handler
// returns those nodes and sets included_staging=true. Otherwise it falls
// through to the promoted store via GraphStorage.NodesForFile.
//
// solov2-8ex retired the previous in-handler type-assertion to an optional
// fileQuerier interface; NodesForFile is now part of the port contract.
func makeGetFileNodesHandler(graph ports.GraphStorage, staging *application.StagingArea) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getFileNodesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if p.FilePath == "" || p.RepoID == "" || p.Branch == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "file_path, repo_id, and branch are required"}
		}

		// Staging overlay wins when present.
		if stagedNodes, ok := staging.GetStagedNodes(p.RepoID, p.Branch, p.FilePath); ok {
			return GraphResponse{Nodes: stagedNodes, IncludedStaging: true}, nil
		}

		nodes, err := graph.NodesForFile(ctx, p.RepoID, p.Branch, p.FilePath)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query failed: %v", err)}
		}
		return GraphResponse{Nodes: nodes, IncludedStaging: false}, nil
	}
}
