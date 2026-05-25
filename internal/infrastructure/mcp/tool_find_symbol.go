package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// isContainerKind reports whether a node kind is a structural container rather
// than a callable/declaration symbol. Container nodes (package/file/module/
// chunk) carry no CALLS edges, so eng_find_symbol ranks them below real
// declarations for the same name (solov2-rd0l).
func isContainerKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindPackage, domain.KindFile, domain.KindModule, domain.KindChunk:
		return true
	default:
		return false
	}
}

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
		if rpcErr := checkRequired("symbol", p.Symbol); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ktz0: allow repo_id to be omitted when the shim-injected cwd
		// matches a registered repo. Falls back to the single-repo case when
		// only one repo is registered.
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

		// Deterministic ranking (merged is a map, so iteration order is
		// otherwise random). Exact-name matches first; then declaration /
		// callable kinds ahead of container kinds (package/file/module/chunk)
		// — a caller taking nodes[0] for call_chain/blast_radius wants the
		// function "main", not the package "main" (solov2-rd0l). Name then
		// node_id break ties so output is stable.
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
			return a.ID < b.ID
		})

		return GraphResponse{
			Nodes:           nodesToDTO(result),
			IncludedStaging: includedStaging,
		}, nil
	}
}
