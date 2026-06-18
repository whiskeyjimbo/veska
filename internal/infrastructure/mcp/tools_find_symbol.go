// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
)

// isContainerKind reports whether a node kind is a structural container (package/file/module/chunk) to rank them below callable symbols during searches.
func isContainerKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindPackage, domain.KindFile, domain.KindModule, domain.KindChunk:
		return true
	default:
		return false
	}
}

type findSymbolParams struct {
	Symbol string `json:"symbol"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	Kind   string `json:"kind,omitempty"`
}

func makeFindSymbolHandler(graph ports.GraphReader, staging *staging.Area, repos application.RepoLister, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findSymbolParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if rpcErr := checkRequired("symbol", p.Symbol); rpcErr != nil {
			return nil, rpcErr
		}
		// If repo_id is omitted, query targets fan out across all registered repositories.
		targets, fanout, rpcErr := resolveRepoFanoutFromParams(ctx, repos, raw, p.RepoID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}

		type mergeKey struct {
			repoID string
			nodeID domain.NodeID
		}
		type hit struct {
			node   *domain.Node
			repoID string
		}
		merged := make(map[mergeKey]hit)
		includedStaging := false

		for _, tgt := range targets {
			promoted, err := graph.FindNodes(ctx, tgt.RepoID, tgt.Branch, p.Symbol)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("graph lookup failed: %v", err)}
			}
			for _, n := range promoted {
				merged[mergeKey{tgt.RepoID, n.ID}] = hit{node: n, repoID: tgt.RepoID}
			}
			for _, fp := range staging.StagedFiles(tgt.RepoID, tgt.Branch) {
				stagedNodes, ok := staging.GetStagedNodes(tgt.RepoID, tgt.Branch, fp)
				if !ok {
					continue
				}
				for _, sn := range stagedNodes {
					if sn.Name != p.Symbol {
						continue
					}
					merged[mergeKey{tgt.RepoID, sn.ID}] = hit{node: sn, repoID: tgt.RepoID}
					includedStaging = true
				}
			}
		}

		result := make([]*domain.Node, 0, len(merged))
		repoByNode := make(map[domain.NodeID]string, len(merged))
		for k, h := range merged {
			if p.Kind != "" && string(h.node.Kind) != p.Kind {
				continue
			}
			result = append(result, h.node)
			repoByNode[k.nodeID] = h.repoID
		}

		// Results are sorted deterministically, placing callable definitions before container nodes and breaking ties by name, repository ID, and node ID.
		sort.SliceStable(result, func(i, j int) bool {
			a, b := result[i], result[j]
			if ae, be := a.Name == p.Symbol, b.Name == p.Symbol; ae != be {
				return ae
			}
			if ac, bc := isContainerKind(a.Kind), isContainerKind(b.Kind); ac != bc {
				return !ac
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			if ra, rb := repoByNode[a.ID], repoByNode[b.ID]; ra != rb {
				return ra < rb
			}
			return a.ID < b.ID
		})

		dtos := nodesToDTO(result)
		if fanout {
			// Repository IDs are stamped only in multi-repository (fanout) scenarios to preserve single-repo schemas.
			for i, n := range dtos {
				dtos[i].RepoID = repoByNode[domain.NodeID(n.NodeID)]
			}
		}
		reasons := []string{}
		var indexing []string
		// Empty responses during an active cold scan return an indexing in-progress degraded reason.
		if len(dtos) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}

		queriedRepos := make([]string, 0, len(targets))
		for _, tgt := range targets {
			queriedRepos = append(queriedRepos, tgt.RepoID)
		}
		reconciling := reconcilingForRepos(reconcile, queriedRepos)
		if len(reconciling) > 0 {
			reasons = append(reasons, protocol.DegradedReasonWakeReconciling)
		}
		return GraphResponse{
			Nodes:                dtos,
			IncludedStaging:      includedStaging,
			DegradedReasons:      reasons,
			IndexingRepos:        indexing,
			WakeReconcilingRepos: reconciling,
		}, nil
	}
}
