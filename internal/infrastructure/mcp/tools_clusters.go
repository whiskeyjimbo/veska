// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type findClustersParams struct {
	RepoID   string  `json:"repo_id"`
	Branch   string  `json:"branch"`
	Scope    string  `json:"scope"`
	Tiers    string  `json:"tiers"`
	MinScore float32 `json:"min_score"`
	Path     string  `json:"path"`
	Limit    int     `json:"limit,omitempty"`
}

// clusterMemberDTO represents one symbol in a cluster, containing RepoID so cross-repo matches are actionable.
type clusterMemberDTO struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	RepoID    string `json:"repo_id"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// clusterDTO is a group of similar symbols, where Score is set only for the near tier to represent the weakest SIMILAR_TO link in the component.
type clusterDTO struct {
	Tier      string             `json:"tier"`
	Size      int                `json:"size"`
	Score     float32            `json:"score,omitempty"`
	CrossRepo bool               `json:"cross_repo"`
	Members   []clusterMemberDTO `json:"members"`
}

// FindClustersResponse is the response envelope where Clusters is guaranteed non-null to prevent serialization issues.
// Total is the full unclamped cluster count; Truncated is true when the page was capped.
type FindClustersResponse struct {
	Scope     string       `json:"scope"`
	Clusters  []clusterDTO `json:"clusters"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
}

func makeFindClustersHandler(finder CloneFinder, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var p findClustersParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		scope := p.Scope
		if scope == "" {
			scope = "repo"
		}
		if scope != "repo" && scope != "all" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("scope must be 'repo' or 'all', got %q", p.Scope)}
		}
		tiers, rpcErr := parseTiers(p.Tiers)
		if rpcErr != nil {
			return nil, rpcErr
		}

		opts := duplicates.ClusterOptions{
			Branch: p.Branch, PathPrefix: p.Path, Tiers: tiers, MinScore: p.MinScore,
		}
		if scope == "all" {
			opts.AllRepos = true
			if opts.Branch == "" {
				opts.Branch = "main"
			}
		} else {
			repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
			if rpcErr != nil {
				return nil, rpcErr
			}
			branch, rpcErr := resolveBranchOrActive(ctx, repos, repoID, p.Branch)
			if rpcErr != nil {
				return nil, rpcErr
			}
			opts.RepoID, opts.Branch = repoID, branch
		}

		clusters, err := finder.Clusters(ctx, opts)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_clusters: %v", err)}
		}
		resp := FindClustersResponse{Scope: scope, Total: len(clusters)}
		limit := clampListLimit(p.Limit)
		if len(clusters) > limit {
			clusters = clusters[:limit]
			resp.Truncated = true
		}
		resp.Clusters = clustersToDTO(clusters)
		return resp, nil
	}
}

func parseTiers(csv string) ([]duplicates.Tier, *RPCError) {
	if strings.TrimSpace(csv) == "" {
		return nil, nil
	}
	valid := map[string]duplicates.Tier{
		"exact":      duplicates.TierExact,
		"structural": duplicates.TierStructural,
		"near":       duplicates.TierNear,
	}
	var out []duplicates.Tier
	for raw := range strings.SplitSeq(csv, ",") {
		t, ok := valid[strings.TrimSpace(raw)]
		if !ok {
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown tier %q (want exact|structural|near)", strings.TrimSpace(raw))}
		}
		out = append(out, t)
	}
	return out, nil
}

func clustersToDTO(in []duplicates.Cluster) []clusterDTO {
	out := make([]clusterDTO, 0, len(in))
	for _, c := range in {
		members := make([]clusterMemberDTO, 0, len(c.Members))
		for _, m := range c.Members {
			members = append(members, clusterMemberDTO{
				NodeID: m.NodeID, Name: m.SymbolPath, Kind: m.Kind, RepoID: m.RepoID,
				FilePath: m.FilePath, LineStart: m.LineStart, LineEnd: m.LineEnd,
			})
		}
		out = append(out, clusterDTO{
			Tier: string(c.Tier), Size: c.Size, Score: c.Score,
			CrossRepo: c.CrossRepo, Members: members,
		})
	}
	return out
}
