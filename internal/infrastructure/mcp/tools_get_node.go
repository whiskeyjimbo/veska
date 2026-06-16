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

// eng_get_node

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

		// Resolve a node_id prefix (e.g. the 12-char display id that
		// eng_find_symbol / `veska symbol` print) to its full id up front, so
		// both the scoped (repo_id supplied) and global lookup paths below run
		// against an exact id. A full 64-char id resolves to
		// itself; an ambiguous prefix errors with the candidate ids.
		resolvedID, rpcErr := resolveNodeIDPrefix(ctx, graph, p.NodeID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.NodeID = resolvedID

		// node_id is a content-hashed sha256, globally unique by construction,
		// so repo_id+branch are not needed to locate the node. The scoped path
		// is taken whenever the caller supplied repo_id — branch defaults to
		// that repo's active_branch. Previously, supplying
		// repo_id without branch silently dropped to the cross-repo fallback
		// and ignored repo_id; an unknown or mistyped repo_id never surfaced
		// to the caller. Only when both repo_id and branch are absent does
		// the handler take the global FindNodeByID path.
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
			// not-found is a domain error, not a malformed-params error.
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node not found: %s", p.NodeID)}
		}

		return GraphResponse{
			Nodes:           nodesToDTO([]*domain.Node{node}),
			IncludedStaging: includedStaging,
			DegradedReasons: []string{},
		}, nil
	}
}

// nodePrefixMinLen is the shortest accepted node_id prefix. The display id
// eng_find_symbol prints is 12 hex chars; we accept down to this floor and lean
// on the ambiguity guard for everything else. A shorter input is treated as an
// exact id (resolveNodeIDPrefix is a no-op) so legitimately short test ids and
// any non-hex literal still match exactly.
const nodePrefixMinLen = 8

// resolveNodeIDPrefix maps a node_id prefix to its full id. A full 64-char id
// resolves to itself (it is its own unique prefix). When the input is shorter
// than nodePrefixMinLen it is returned unchanged so the downstream exact-match
// path runs as before. Zero matches also pass through unchanged so the existing
// not-found / staging-overlay handling owns that case. An ambiguous prefix
// (more than one distinct node_id matches) is a CodeInvalidParams error listing
// the candidates, so the caller can disambiguate.
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
			"ambiguous node_id prefix %q matches multiple nodes (e.g. %s, %s) — supply more characters",
			nodeID, ids[0], ids[1])}
	}
}
