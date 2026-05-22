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
// eng_find_symbol
// ---------------------------------------------------------------------------

type findSymbolParams struct {
	Symbol string `json:"symbol"`
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	Kind   string `json:"kind,omitempty"`
}

func makeFindSymbolHandler(graph ports.GraphStorage, staging *application.StagingArea, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findSymbolParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if rpcErr := checkRequired("symbol", p.Symbol, "repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}
		repoID, rpcErr := resolveRepoID(ctx, repos, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID

		promoted, err := graph.FindNodes(ctx, p.RepoID, p.Branch, p.Symbol)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("graph lookup failed: %v", err)}
		}

		// Build a map of promoted nodes keyed by ID for merge.
		merged := make(map[domain.NodeID]*domain.Node, len(promoted))
		for _, n := range promoted {
			merged[n.ID] = n
		}

		// Overlay staged nodes from all files that contain the symbol.
		includedStaging := false
		stagedFiles := staging.StagedFiles(p.RepoID, p.Branch)
		for _, fp := range stagedFiles {
			stagedNodes, ok := staging.GetStagedNodes(p.RepoID, p.Branch, fp)
			if !ok {
				continue
			}
			for _, sn := range stagedNodes {
				if sn.Name != p.Symbol {
					continue
				}
				merged[sn.ID] = sn // staged overrides promoted
				includedStaging = true
			}
		}

		// Apply optional kind filter and build result slice.
		result := make([]*domain.Node, 0, len(merged))
		for _, n := range merged {
			if p.Kind != "" && string(n.Kind) != p.Kind {
				continue
			}
			result = append(result, n)
		}

		return GraphResponse{
			Nodes:           nodesToDTO(result),
			IncludedStaging: includedStaging,
		}, nil
	}
}
