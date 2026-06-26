// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type CloneFinder interface {
	ExactClones(ctx context.Context, repoID, branch string) ([]duplicates.CloneGroup, error)
	NearDuplicates(ctx context.Context, repoID, branch string, minScore float32) ([]duplicates.NearCluster, error)
	Clusters(ctx context.Context, opts duplicates.ClusterOptions) ([]duplicates.Cluster, error)
}

type findClonesParams struct {
	RepoID   string  `json:"repo_id"`
	Branch   string  `json:"branch"`
	Mode     string  `json:"mode"`
	MinScore float32 `json:"min_score"`
	Limit    int     `json:"limit,omitempty"`
}

type cloneMemberDTO struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

type cloneGroupDTO struct {
	ContentHash string           `json:"content_hash"`
	Size        int              `json:"size"`
	Members     []cloneMemberDTO `json:"members"`
}

type nearClusterDTO struct {
	Size     int              `json:"size"`
	MinScore float32          `json:"min_score"`
	MaxScore float32          `json:"max_score"`
	Members  []cloneMemberDTO `json:"members"`
}

// FindClonesResponse returns duplicate code groups or clusters, defaulting collections to empty slices to ensure safe client serialization.
// Total is the full unclamped count of the active mode's collection; Truncated is true when the page was capped.
type FindClonesResponse struct {
	Mode      string           `json:"mode"`
	Groups    []cloneGroupDTO  `json:"groups"`
	Clusters  []nearClusterDTO `json:"clusters"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
}

// eng_find_clones and eng_find_clusters are no longer registered as standalone
// tools - they merged into eng_find_duplicates (seed=clones / seed=clusters),
// registered by RegisterDuplicatesTool, which reuses makeFindClonesHandler /
// makeFindClustersHandler below.

func makeFindClonesHandler(finder CloneFinder, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findClonesParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		mode := p.Mode
		if mode == "" {
			mode = "exact"
		}
		if mode != "exact" && mode != "near" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("mode must be 'exact' or 'near', got %q", p.Mode)}
		}
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		branch, rpcErr := resolveBranchOrActive(ctx, repos, repoID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}

		limit := clampListLimit(p.Limit)
		resp := FindClonesResponse{Mode: mode, Groups: []cloneGroupDTO{}, Clusters: []nearClusterDTO{}}
		if mode == "near" {
			clusters, err := finder.NearDuplicates(ctx, repoID, branch, p.MinScore)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_clones: %v", err)}
			}
			resp.Total = len(clusters)
			if len(clusters) > limit {
				clusters = clusters[:limit]
				resp.Truncated = true
			}
			resp.Clusters = nearClustersToDTO(clusters)
			return resp, nil
		}
		groups, err := finder.ExactClones(ctx, repoID, branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_clones: %v", err)}
		}
		resp.Total = len(groups)
		if len(groups) > limit {
			groups = groups[:limit]
			resp.Truncated = true
		}
		resp.Groups = cloneGroupsToDTO(groups)
		return resp, nil
	}
}

func cloneMembersToDTO(in []duplicates.CloneMember) []cloneMemberDTO {
	out := make([]cloneMemberDTO, 0, len(in))
	for _, m := range in {
		out = append(out, cloneMemberDTO{
			NodeID:    m.NodeID,
			Name:      m.SymbolPath,
			Kind:      m.Kind,
			FilePath:  m.FilePath,
			LineStart: m.LineStart,
			LineEnd:   m.LineEnd,
		})
	}
	return out
}

func cloneGroupsToDTO(groups []duplicates.CloneGroup) []cloneGroupDTO {
	out := make([]cloneGroupDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, cloneGroupDTO{ContentHash: g.ContentHash, Size: g.Size, Members: cloneMembersToDTO(g.Members)})
	}
	return out
}

func nearClustersToDTO(clusters []duplicates.NearCluster) []nearClusterDTO {
	out := make([]nearClusterDTO, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, nearClusterDTO{Size: c.Size, MinScore: c.MinScore, MaxScore: c.MaxScore, Members: cloneMembersToDTO(c.Members)})
	}
	return out
}
