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

// Default git refs for eng_find_changed_symbols when the caller omits both:
// the most recent commit against its parent — the common "what did the last
// commit change?" query (solov2-npjs).
const (
	defaultChangedRefA = "HEAD~1"
	defaultChangedRefB = "HEAD"
	// gitEmptyTreeSHA is git's canonical empty-tree object. Diffing HEAD
	// against it yields every file in HEAD as an addition — the right
	// answer for a single-commit repo where HEAD~1 doesn't exist
	// (solov2-wrbn).
	gitEmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
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
func RegisterChangedSymbolsTool(r *Registry, svc *changedsymbols.Service, repoRoot RepoRootFunc, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:            "eng_find_changed_symbols",
		Description:     "Symbol-grain diff between two git refs — answers 'which functions/methods/structs actually changed?' for PR review, blame, or 'why did this break since yesterday'. ref_a/ref_b (aliases base/head) default to HEAD~1..HEAD. Comment- or whitespace-only changes emit a 'non_symbol_changes_only' degraded_reason so callers know the file changed even when no symbol diff comes back (solov2-u9os). Pair with eng_get_diff_blast_radius for 'what's downstream of these changes'.",
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
	// Base/Head are git's canonical names for ref_a/ref_b. Accepting
	// both keeps the journey-natural call (base/head) working alongside
	// the legacy ref_a/ref_b; either alias is honoured but supplying
	// conflicting values is rejected (solov2-3ocy).
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
		// solov2-3ocy: collapse base/head aliases into ref_a/ref_b. Reject
		// only when both names are supplied with DIFFERENT non-empty values
		// — equality is fine since some agents may set both for safety.
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
		// ref_a/ref_b default to the last commit (HEAD~1..HEAD) when both are
		// omitted; supplying only one is ambiguous and rejected (solov2-npjs).
		usedDefaults := false
		switch {
		case p.RefA == "" && p.RefB == "":
			p.RefA, p.RefB = defaultChangedRefA, defaultChangedRefB
			usedDefaults = true
		case p.RefA == "" || p.RefB == "":
			return nil, &RPCError{Code: CodeInvalidParams, Message: "ref_a and ref_b must be provided together (or both omitted to default to HEAD~1..HEAD)"}
		}
		// solov2-ktz0: shim-injected cwd resolves repo_id when omitted.
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
		// solov2-wrbn: on a single-commit repo the default HEAD~1 doesn't
		// resolve. Retry against git's canonical empty-tree SHA so every
		// symbol in HEAD comes back as "added" instead of the user seeing
		// a self-contradicting "try omitting both refs" error on a path
		// where they already did. Only fires when we picked the defaults —
		// caller-supplied refs that don't resolve are still their problem.
		if err != nil && usedDefaults && errors.Is(err, gitinfra.ErrUnknownRevision) {
			p.RefA = gitEmptyTreeSHA
			res, err = svc.Diff(ctx, p.RepoID, root, p.RefA, p.RefB)
		}
		// Rewrite file_path to absolute form so it matches the contract used
		// by every other node-emitting tool (eng_find_symbol,
		// eng_get_file_nodes, etc.). The service stores repo-relative paths
		// because git diff yields them that way; the wire surface must be
		// uniform (solov2-w8nr).
		absolutiseChangedSymbols := func(slice []changedsymbols.SymbolChange) {
			for i := range slice {
				if slice[i].FilePath != "" && !filepath.IsAbs(slice[i].FilePath) {
					slice[i].FilePath = filepath.Join(root, slice[i].FilePath)
				}
			}
		}
		if err != nil {
			// An unresolvable ref (most commonly HEAD~1 on a single-commit
			// repo) is a caller problem, not an internal failure — return a
			// friendly invalid-params instead of leaking raw git stderr
			// (solov2-dr31).
			if errors.Is(err, gitinfra.ErrUnknownRevision) {
				return nil, &RPCError{
					Code:    CodeInvalidParams,
					Message: fmt.Sprintf("ref_a=%q or ref_b=%q does not resolve in the repo (insufficient history? try omitting both refs or pass an explicit pair)", p.RefA, p.RefB),
				}
			}
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find changed symbols: %v", err)}
		}
		absolutiseChangedSymbols(res.Added)
		absolutiseChangedSymbols(res.Removed)
		absolutiseChangedSymbols(res.Modified)
		return res, nil
	}
}
