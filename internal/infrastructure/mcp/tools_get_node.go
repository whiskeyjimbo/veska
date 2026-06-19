// SPDX-License-Identifier: AGPL-3.0-only

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

		// Node ID prefixes are expanded to full IDs before lookup to support partial matching from CLI queries.
		resolvedID, rpcErr := resolveNodeIDPrefix(ctx, graph, p.NodeID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.NodeID = resolvedID

		// Look up node by ID within the specified repository/branch if RepoID is provided; otherwise, search globally.
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

			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node not found: %s", p.NodeID)}
		}

		return GraphResponse{
			Nodes:           nodesToDTO([]*domain.Node{node}),
			IncludedStaging: includedStaging,
			DegradedReasons: []string{},
		}, nil
	}
}

// nodePrefixMinLen is the minimum length required to trigger prefix expansion for node IDs.
const nodePrefixMinLen = 8

// resolveNodeIDPrefix expands a partial node ID to its full ID, returning an error if the prefix is ambiguous.
func resolveNodeIDPrefix(ctx context.Context, graph ports.GraphReader, nodeID string) (string, *RPCError) {
	if len(nodeID) < nodePrefixMinLen {
		return nodeID, nil
	}
	ids, err := graph.FindNodeIDsByPrefix(ctx, nodeID, 2)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("node prefix lookup failed: %v", err)}
	}
	switch len(ids) {
	case 0:
		return nodeID, nil
	case 1:
		return string(ids[0]), nil
	default:
		return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf(
			"ambiguous node_id prefix %q matches multiple nodes (e.g. %s, %s) - supply more characters",
			nodeID, ids[0], ids[1])}
	}
}
