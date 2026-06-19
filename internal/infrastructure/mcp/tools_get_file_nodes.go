// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

type getFileNodesParams struct {
	FilePath string `json:"file_path"`
	// Path is supported as an alias for FilePath to handle caller variation.
	Path   string `json:"path"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

// makeGetFileNodesHandler retrieves all nodes for a given file path, prioritizing staged nodes if present.
func makeGetFileNodesHandler(graph ports.GraphReader, staging *staging.Area, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getFileNodesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		filePath := p.FilePath
		if filePath == "" {
			filePath = p.Path
		}
		if filePath == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "file_path (or path) is required"}
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

		// Normalize caller paths to repo-relative slash format to match the stored paths format.
		filePath = toStoredPath(ctx, repos, p.RepoID, filePath)

		if stagedNodes, ok := staging.GetStagedNodes(p.RepoID, p.Branch, filePath); ok {
			return GraphResponse{Nodes: nodesToDTO(stagedNodes), IncludedStaging: true, DegradedReasons: []string{}}, nil
		}

		nodes, err := graph.NodesForFile(ctx, p.RepoID, p.Branch, filePath)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("query failed: %v", err)}
		}
		return GraphResponse{Nodes: nodesToDTO(nodes), IncludedStaging: false, DegradedReasons: []string{}}, nil
	}
}

// toStoredPath normalizes a file path to the repository-relative slash format, falling back to the input path on failure.
func toStoredPath(ctx context.Context, repos application.RepoLister, repoID, p string) string {
	if filepath.IsAbs(p) && repos != nil {
		if root, ok := repoRoot(ctx, repos, repoID); ok {
			if rel, err := filepath.Rel(root, p); err == nil {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(p)
}

// repoRoot retrieves the absolute path of the repository root, returning false if the repository is not registered.
func repoRoot(ctx context.Context, repos application.RepoLister, repoID string) (string, bool) {
	if repos == nil {
		return "", false
	}
	all, err := repos.ListRepos(ctx)
	if err != nil {
		return "", false
	}
	for _, rec := range all {
		if rec.RepoID == repoID {
			return rec.RootPath, true
		}
	}
	return "", false
}
