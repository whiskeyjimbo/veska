package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// This file holds the eng_find_clones handler: exact-clone detection by
// content_hash equality (solov2-wfrj). It is the deterministic, embedding-free
// half of duplicate detection — for the per-function "is THIS duplicated?"
// pivot use eng_search_similar instead (noted in the tool description).

// CloneFinder is the narrow port the handler needs from the duplicates
// application service. *duplicates.Finder satisfies it structurally.
type CloneFinder interface {
	ExactClones(ctx context.Context, repoID, branch string) ([]duplicates.CloneGroup, error)
	NearDuplicates(ctx context.Context, repoID, branch string, minScore float32) ([]duplicates.NearCluster, error)
}

// DescFindClones is the eng_find_clones tool description. It states the exact
// semantics (byte-identical copy-paste, not "similar"), the near mode (fuzzy
// clusters over persisted similarity edges), and points the agent at
// eng_search_similar for the single-function question.
const DescFindClones = "Find duplicate code. mode='exact' (default): groups of >=2 symbols whose source text is byte-for-byte identical (literal copy-paste), detected by content_hash equality — deterministic, no embeddings. mode='near': clusters of symbols whose persisted SIMILAR_TO similarity exceeds a threshold higher than auto-link's 'related' cutoff (fuzzy near-duplicates — renamed copies, drifted variants); reads scores auto-link already stored, runs no new similarity sweep. For 'what else looks LIKE this ONE symbol?' use eng_search_similar instead. Container/sub-symbol kinds (package, chunk, file, module, field, import) are excluded so boilerplate doesn't flood results. NOTE: near mode needs SIMILAR_TO edges carrying a score, which only exist after a promotion/reindex on a build with the score column — older indexes report no near clusters until reindexed."

var findClonesInputSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Find duplicate code in a repo/branch. mode=exact returns 'groups' (byte-identical via content_hash); mode=near returns 'clusters' (fuzzy, thresholded SIMILAR_TO edges).",
  "properties": {
    "repo_id":   {"type": "string", "description": "Repo to scan. Resolved from cwd when omitted."},
    "branch":    {"type": "string", "description": "Branch to scan. Defaults to the repo's active branch."},
    "mode":      {"type": "string", "enum": ["exact", "near"], "description": "exact (default): byte-identical clones via content_hash, populates 'groups'. near: fuzzy clusters from thresholded SIMILAR_TO edges, populates 'clusters'."},
    "min_score": {"type": "number", "description": "near mode only: minimum SIMILAR_TO edge score (higher = more similar). Omit to use the default calibrated for the elected embedder (model spaces differ; near-dup and 'related' bands overlap, so this is a high-precision/partial-recall knob). Lower it for more recall."},
    "cwd":       {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

type findClonesParams struct {
	RepoID   string  `json:"repo_id"`
	Branch   string  `json:"branch"`
	Mode     string  `json:"mode"`
	MinScore float32 `json:"min_score"`
}

// cloneMemberDTO is one occurrence of a clone on the wire.
type cloneMemberDTO struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// cloneGroupDTO is one set of byte-identical copies (mode=exact).
type cloneGroupDTO struct {
	ContentHash string           `json:"content_hash"`
	Size        int              `json:"size"`
	Members     []cloneMemberDTO `json:"members"`
}

// nearClusterDTO is one set of fuzzily-similar symbols (mode=near). Score
// bounds describe the weakest/strongest SIMILAR_TO link inside the cluster.
type nearClusterDTO struct {
	Size     int              `json:"size"`
	MinScore float32          `json:"min_score"`
	MaxScore float32          `json:"max_score"`
	Members  []cloneMemberDTO `json:"members"`
}

// FindClonesResponse is the eng_find_clones envelope. Exactly one of Groups
// (mode=exact) / Clusters (mode=near) is populated per call; both are non-null
// so clients can range over either unconditionally. Mode echoes the resolved
// mode so the caller knows which field to read.
type FindClonesResponse struct {
	Mode     string           `json:"mode"`
	Groups   []cloneGroupDTO  `json:"groups"`
	Clusters []nearClusterDTO `json:"clusters"`
}

// RegisterCloneTools registers eng_find_clones. finder + repos are required;
// the tool is skipped by callers that pass a nil finder (legacy/test wiring).
func RegisterCloneTools(r *Registry, finder CloneFinder, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_clones",
		Description:     DescFindClones,
		IncludesStaging: false,
		InputSchema:     findClonesInputSchema,
		Handler:         makeFindClonesHandler(finder, repos),
	})
}

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

		resp := FindClonesResponse{Mode: mode, Groups: []cloneGroupDTO{}, Clusters: []nearClusterDTO{}}
		if mode == "near" {
			clusters, err := finder.NearDuplicates(ctx, repoID, branch, p.MinScore)
			if err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_clones: %v", err)}
			}
			resp.Clusters = nearClustersToDTO(clusters)
			return resp, nil
		}
		groups, err := finder.ExactClones(ctx, repoID, branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_clones: %v", err)}
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
