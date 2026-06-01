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

// isContainerKind reports whether a node kind is a structural container rather
// than a callable/declaration symbol. Container nodes (package/file/module/
// chunk) carry no CALLS edges, so eng_find_symbol ranks them below real
// declarations for the same name .
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

func makeFindSymbolHandler(graph ports.GraphReader, staging *staging.Area, repos application.RepoLister, scans ScanTrackerReader, reconcile ReconcileReader) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findSymbolParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
		}
		if rpcErr := checkRequired("symbol", p.Symbol); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-g8fh: when repo_id is omitted and cwd doesn't match any
		// registered repo, fan out across every repo instead of erroring.
		// Single-repo callers (the common case) still get a one-target
		// result identical to the pre-fanout behaviour.
		targets, fanout, rpcErr := resolveRepoFanoutFromParams(ctx, repos, raw, p.RepoID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}

		// merged keys by (repo_id, node_id) so the same node_id appearing in
		// two different repos is preserved as two distinct hits.
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

		// Deterministic ranking (merged is a map, so iteration order is
		// otherwise random). Exact-name matches first; then declaration /
		// callable kinds ahead of container kinds (package/file/module/chunk)
		// — a caller taking nodes[0] for call_chain/blast_radius wants the
		// function "main", not the package "main" . Name, then
		// repo_id (for fanout), then node_id break ties so output is stable.
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
			// solov2-g8fh: only stamp repo_id when the response actually
			// spans repos — single-repo responses keep the pre-fanout shape.
			for i, n := range dtos {
				dtos[i].RepoID = repoByNode[domain.NodeID(n.NodeID)]
			}
		}
		reasons := []string{}
		var indexing []string
		// solov2-izh6.30: empty result during an active cold scan is the
		// classic "junior just registered the repo and queried" race. Tell
		// the caller so they retry instead of concluding the symbol doesn't
		// exist. The hint fires only on empty responses — a non-empty hit
		// is authoritative even if some OTHER repo is still indexing.
		if len(dtos) == 0 {
			if ids, busy := indexingRepoIDs(scans); busy {
				reasons = append(reasons, protocol.DegradedReasonIndexingInProgress)
				indexing = ids
			}
		}
		// wake_reconciling fires on empty AND non-empty results whenever a
		// queried repo's suspend/resume sweep is mid-flight (solov2-xde2.25.1).
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
