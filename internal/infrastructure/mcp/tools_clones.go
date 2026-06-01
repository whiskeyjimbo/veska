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
}

// DescFindClones is the eng_find_clones tool description. It states the exact
// semantics (byte-identical copy-paste, not "similar") and points the agent at
// eng_search_similar for the single-function question, so the two surfaces stay
// disjoint rather than overlapping.
const DescFindClones = "Find EXACT code clones: groups of >=2 symbols whose source text is byte-for-byte identical (literal copy-paste), detected by content_hash equality — deterministic, no embeddings. Use to locate copy-paste that should be extracted into a shared helper. This is NOT fuzzy similarity: for 'what else looks LIKE this one symbol?' use eng_search_similar instead. Container/sub-symbol kinds (package, chunk, file, module, field, import) are excluded so boilerplate doesn't flood results."

var findClonesInputSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Find exact (byte-identical) code-clone groups in a repo/branch via content_hash equality.",
  "properties": {
    "repo_id": {"type": "string", "description": "Repo to scan. Resolved from cwd when omitted."},
    "branch":  {"type": "string", "description": "Branch to scan. Defaults to the repo's active branch."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

type findClonesParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
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

// cloneGroupDTO is one set of byte-identical copies.
type cloneGroupDTO struct {
	ContentHash string           `json:"content_hash"`
	Size        int              `json:"size"`
	Members     []cloneMemberDTO `json:"members"`
}

// FindClonesResponse is the eng_find_clones envelope. Groups is never null so
// clients can range over it unconditionally.
type FindClonesResponse struct {
	Groups []cloneGroupDTO `json:"groups"`
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
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		branch, rpcErr := resolveBranchOrActive(ctx, repos, repoID, p.Branch)
		if rpcErr != nil {
			return nil, rpcErr
		}

		groups, err := finder.ExactClones(ctx, repoID, branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find_clones: %v", err)}
		}
		return FindClonesResponse{Groups: cloneGroupsToDTO(groups)}, nil
	}
}

func cloneGroupsToDTO(groups []duplicates.CloneGroup) []cloneGroupDTO {
	out := make([]cloneGroupDTO, 0, len(groups))
	for _, g := range groups {
		members := make([]cloneMemberDTO, 0, len(g.Members))
		for _, m := range g.Members {
			members = append(members, cloneMemberDTO{
				NodeID:    m.NodeID,
				Name:      m.SymbolPath,
				Kind:      m.Kind,
				FilePath:  m.FilePath,
				LineStart: m.LineStart,
				LineEnd:   m.LineEnd,
			})
		}
		out = append(out, cloneGroupDTO{ContentHash: g.ContentHash, Size: g.Size, Members: members})
	}
	return out
}
