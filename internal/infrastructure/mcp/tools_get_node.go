package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
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

func makeGetNodeHandler(graph ports.GraphReader, staging *staging.Area, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p getNodeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if rpcErr := checkRequired("node_id", p.NodeID); rpcErr != nil {
			return nil, rpcErr
		}

		// node_id is a content-hashed sha256, globally unique by construction,
		// so repo_id+branch are not needed to locate the node. The scoped path
		// is taken whenever the caller supplied repo_id — branch defaults to
		// that repo's active_branch (solov2-hb2s). Previously, supplying
		// repo_id without branch silently dropped to the cross-repo fallback
		// and ignored repo_id; an unknown or mistyped repo_id never surfaced
		// to the caller. Only when both repo_id and branch are absent does
		// the handler take the global FindNodeByID path (solov2-v4ob).
		var (
			node            *domain.Node
			err             error
			includedStaging bool
		)
		if p.RepoID != "" {
			repoID, rpcErr := resolveRepoID(ctx, repos, p.RepoID)
			if rpcErr != nil {
				return nil, rpcErr
			}
			p.RepoID = repoID
			branch, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch)
			if rpcErr != nil {
				return nil, rpcErr
			}
			p.Branch = branch
			node, err = graph.GetNode(ctx, p.RepoID, p.Branch, domain.NodeID(p.NodeID))
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("graph lookup failed: %v", err)}
			}
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
		} else {
			node, err = graph.FindNodeByID(ctx, domain.NodeID(p.NodeID))
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("graph lookup failed: %v", err)}
			}
		}

		if node == nil {
			// solov2-byxy: not-found is a domain error, not a malformed-params error.
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node not found: %s", p.NodeID)}
		}

		return GraphResponse{
			Nodes:           nodesToDTO([]*domain.Node{node}),
			IncludedStaging: includedStaging,
			DegradedReasons: []string{},
		}, nil
	}
}
