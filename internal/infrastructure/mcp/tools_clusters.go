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

const DescFindClusters = "Whole-repo (or cross-repo) similar-code clusters for de-dupe triage. One pass returns groups of >=2 symbols at three tiers, ranked tightest first: 'exact' (byte-identical copy-paste, content_hash), 'structural' (same shape after renaming variables/literals - Type-2 clones, structural_hash), and 'near' (vector-similar above the elected embedder's calibrated threshold). A symbol appears at most once, at its tightest tier. No seed needed. scope='all' clusters across EVERY registered repo (exact+structural only - cross-repo near is not yet computed); 'path' narrows to a file_path prefix; 'tiers' selects a subset. Container kinds (package/chunk/file/module/field/import) are excluded. Each cluster's members carry repo_id/file/line so you can open a verify-and-dedupe task per grouping. NOTE: structural/near need structural_hash + scored SIMILAR_TO edges, populated by a promotion/reindex on a current build - reindex older graphs first."

var findClustersInputSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Unified similar-code clusters (exact + structural + near), ranked, for de-dupe task creation. No seed required.",
  "properties": {
    "repo_id":   {"type": "string", "description": "Repo to scan (scope=repo). Resolved from cwd when omitted."},
    "branch":    {"type": "string", "description": "Branch to scan. Defaults to the repo's active branch (scope=repo) or 'main' (scope=all)."},
    "scope":     {"type": "string", "enum": ["repo", "all"], "description": "repo (default): one repo. all: cluster across every registered repo (cross-repo; exact+structural only)."},
    "tiers":     {"type": "string", "description": "Comma-separated subset of exact,structural,near. Omit for all tiers."},
    "min_score": {"type": "number", "description": "near tier only: minimum SIMILAR_TO score. Omit for the elected embedder's calibrated default; lower for more recall."},
    "path":      {"type": "string", "description": "Restrict to nodes whose file_path starts with this prefix (e.g. internal/infrastructure/mcp)."},
    "cwd":       {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted (scope=repo)."}
  }
}`)

type findClustersParams struct {
	RepoID   string  `json:"repo_id"`
	Branch   string  `json:"branch"`
	Scope    string  `json:"scope"`
	Tiers    string  `json:"tiers"`
	MinScore float32 `json:"min_score"`
	Path     string  `json:"path"`
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
type FindClustersResponse struct {
	Scope    string       `json:"scope"`
	Clusters []clusterDTO `json:"clusters"`
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
		return FindClustersResponse{Scope: scope, Clusters: clustersToDTO(clusters)}, nil
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
