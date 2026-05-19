package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ---------------------------------------------------------------------------
// eng_find_changed_symbols
// ---------------------------------------------------------------------------
//
// Given a repo and two git refs, this tool reports the symbols added,
// removed, or modified between them. It parses the changed files at each
// ref on demand and diffs the symbol sets — it does NOT read the promoted
// SQLite graph, so it needs no per-commit history substrate.

// RegisterChangedSymbolsTool registers eng_find_changed_symbols on r.
//
// svc and repoRoot are required; when either is nil the tool is still
// registered but returns InternalError on every call, keeping the
// registry uniform across composition roots that have not wired the
// parser/git adapters.
func RegisterChangedSymbolsTool(r *Registry, svc *changedsymbols.Service, repoRoot RepoRootFunc) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_changed_symbols",
		Description:     "Report symbols added/removed/modified between two git refs by parsing the changed files at each ref.",
		IncludesStaging: false,
		Handler:         makeChangedSymbolsHandler(svc, repoRoot),
	})
}

type changedSymbolsParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	RefA   string `json:"ref_a"`
	RefB   string `json:"ref_b"`
}

func makeChangedSymbolsHandler(svc *changedsymbols.Service, repoRoot RepoRootFunc) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if svc == nil || repoRoot == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "eng_find_changed_symbols is not wired (service or repoRoot missing)",
			}
		}
		var p changedSymbolsParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		if rpcErr := checkRequired(
			"repo_id", p.RepoID, "branch", p.Branch, "ref_a", p.RefA, "ref_b", p.RefB,
		); rpcErr != nil {
			return nil, rpcErr
		}
		root, err := repoRoot(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
		}
		if root == "" {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
		}
		res, err := svc.Diff(ctx, p.RepoID, root, p.RefA, p.RefB)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find changed symbols: %v", err)}
		}
		return res, nil
	}
}
