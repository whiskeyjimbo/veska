// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// Default git refs diff the most recent commit against its parent.
const (
	defaultChangedRefA = "HEAD~1"
	defaultChangedRefB = "HEAD"
	// gitEmptyTreeSHA is diffed against HEAD on a single-commit repository where HEAD~1 does not exist.
	gitEmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// RegisterChangedSymbolsTool registers eng_find_changed_symbols, maintaining registry uniformity even if adapter dependencies are missing.
func RegisterChangedSymbolsTool(r *Registry, svc *changedsymbols.Service, repoRoot RepoRootFunc, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_changed_symbols",
		Description:     "Symbol-grain diff between two git refs - answers 'which functions/methods/structs actually changed?' for PR review, blame, or 'why did this break since yesterday'. ref_a/ref_b (aliases base/head) default to HEAD~1..HEAD. Comment- or whitespace-only changes emit a 'non_symbol_changes_only' degraded_reason so callers know the file changed even when no symbol diff comes back. Pair with eng_get_blast_radius (seed=diff) for 'what's downstream of these changes'.",
		IncludesStaging: false,
		InputSchema:     findChangedSymbolsInputSchema,
		Handler:         makeChangedSymbolsHandler(svc, repoRoot, repos),
	})
}

type changedSymbolsParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	RefA   string `json:"ref_a"`
	RefB   string `json:"ref_b"`
	// Base/Head are aliases for ref_a/ref_b to support standard git nomenclature.
	Base string `json:"base"`
	Head string `json:"head"`
}

func makeChangedSymbolsHandler(svc *changedsymbols.Service, repoRoot RepoRootFunc, repos application.RepoLister) ToolHandler {
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

		if p.Base != "" {
			if p.RefA != "" && p.RefA != p.Base {
				return nil, &RPCError{Code: CodeInvalidParams, Message: "ref_a and base are aliases; supply only one (or matching values)"}
			}
			p.RefA = p.Base
		}
		if p.Head != "" {
			if p.RefB != "" && p.RefB != p.Head {
				return nil, &RPCError{Code: CodeInvalidParams, Message: "ref_b and head are aliases; supply only one (or matching values)"}
			}
			p.RefB = p.Head
		}

		usedDefaults := false
		switch {
		case p.RefA == "" && p.RefB == "":
			p.RefA, p.RefB = defaultChangedRefA, defaultChangedRefB
			usedDefaults = true
		case p.RefA == "" || p.RefB == "":
			return nil, &RPCError{Code: CodeInvalidParams, Message: "ref_a and ref_b must be provided together (or both omitted to default to HEAD~1..HEAD)"}
		}

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
		root, err := repoRoot(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
		}
		if root == "" {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
		}
		res, err := svc.Diff(ctx, p.RepoID, root, p.RefA, p.RefB)
		// For single-commit repositories, diffing defaults fall back to the empty tree hash to avoid resolution errors.
		usedEmptyTreeFallback := false
		if err != nil && usedDefaults && errors.Is(err, gitinfra.ErrUnknownRevision) {
			p.RefA = gitEmptyTreeSHA
			res, err = svc.Diff(ctx, p.RepoID, root, p.RefA, p.RefB)
			usedEmptyTreeFallback = true
		}
		// File paths are converted to absolute format to maintain API consistency with other symbol-retrieval tools.
		absolutiseChangedSymbols := func(slice []changedsymbols.SymbolChange) {
			for i := range slice {
				if slice[i].FilePath != "" && !filepath.IsAbs(slice[i].FilePath) {
					slice[i].FilePath = filepath.Join(root, slice[i].FilePath)
				}
			}
		}
		if err != nil {

			if errors.Is(err, gitinfra.ErrUnknownRevision) {

				aOK := gitinfra.ResolvesRef(ctx, root, p.RefA)
				bOK := gitinfra.ResolvesRef(ctx, root, p.RefB)
				var msg string
				switch {
				case !aOK && !bOK:
					msg = fmt.Sprintf("neither ref_a=%q nor ref_b=%q resolves in this repository - check for typos and verify the refs with `git rev-parse <ref>`", p.RefA, p.RefB)
				case !aOK:
					msg = fmt.Sprintf("ref_a=%q does not resolve in this repository - if you meant 'the parent of HEAD' note this repo has too few commits for that; omit both refs to diff staged+working-tree against HEAD instead", p.RefA)
				case !bOK:
					msg = fmt.Sprintf("ref_b=%q does not resolve in this repository - verify the ref with `git rev-parse %s`", p.RefB, p.RefB)
				default:

					msg = fmt.Sprintf("git diff %s..%s failed despite both refs resolving - try `git diff %s %s` in the repo for details", p.RefA, p.RefB, p.RefA, p.RefB)
				}
				return nil, &RPCError{Code: CodeInvalidParams, Message: msg}
			}
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find changed symbols: %v", err)}
		}
		absolutiseChangedSymbols(res.Added)
		absolutiseChangedSymbols(res.Removed)
		absolutiseChangedSymbols(res.Modified)
		// The empty tree fallback is recorded under degraded reasons to indicate that all symbols are shown as additions.
		if usedEmptyTreeFallback {
			res.DegradedReasons = append(res.DegradedReasons, changedsymbols.DegradedReasonNoParentCommit)
		}
		return res, nil
	}
}
