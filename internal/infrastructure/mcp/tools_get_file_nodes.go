package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ---------------------------------------------------------------------------
// eng_get_file_nodes
// ---------------------------------------------------------------------------

type getFileNodesParams struct {
	FilePath string `json:"file_path"`
	// Path is an accepted alias for FilePath. Node file paths are keyed on
	// "file_path"; "path" is a common caller guess, so we honour both rather
	// than silently returning nothing (solov2-3p1).
	Path   string `json:"path"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

// makeGetFileNodesHandler returns every node for (repo, branch, file_path).
// Staging overlay takes precedence — if the file is in staging the handler
// returns those nodes and sets included_staging=true. Otherwise it falls
// through to the promoted store via GraphStorage.NodesForFile.
//
// solov2-8ex retired the previous in-handler type-assertion to an optional
// fileQuerier interface; NodesForFile is now part of the port contract.
func makeGetFileNodesHandler(graph ports.GraphStorage, staging *application.StagingArea, repos application.RepoLister) ToolHandler {
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
		// solov2-ktz0: shim-injected cwd resolves repo_id when omitted.
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		// Branch defaults to the repo's active branch when omitted (solov2-gp2k),
		// matching find_symbol, get_call_chain, get_blast_radius, et al.
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}

		// Node paths are stored absolute. Resolve a repo-relative file_path
		// against the repo root so callers don't have to pass an absolute path
		// (and don't get a silent empty result when they pass a relative one).
		if !filepath.IsAbs(filePath) && repos != nil {
			if root, ok := repoRoot(ctx, repos, p.RepoID); ok {
				filePath = filepath.Join(root, filePath)
			}
		}

		// Staging overlay wins when present.
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

// repoRoot looks up the absolute working-tree root for repoID. ok is false when
// the repo is unknown or the registry errors — callers then leave the path as
// given rather than failing the request.
func repoRoot(ctx context.Context, repos application.RepoLister, repoID string) (string, bool) {
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
