package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
)

// resolveRepoID validates that repoID names a repo the daemon tracks and
// returns its canonical full id. A short_id prefix (see ShortRepoID) is
// accepted as an alias (solov2-d2x). When no repo matches, a NotFound
// RPCError is returned so a stale or mistyped id surfaces as a loud error
// instead of a silently-empty result (solov2-5rh).
//
// repos may be nil in composition roots that did not wire the registry; in
// that case validation is skipped and repoID is returned unchanged (never
// worse than the pre-validation behaviour).
func resolveRepoID(ctx context.Context, repos application.RepoLister, repoID string) (string, *RPCError) {
	if repos == nil {
		return repoID, nil
	}
	all, err := repos.ListRepos(ctx)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
	}
	for _, rec := range all {
		if rec.RepoID == repoID {
			return rec.RepoID, nil
		}
	}
	// Fall back to a unique short_id prefix match.
	var matched string
	for _, rec := range all {
		if ShortRepoID(rec.RepoID) == repoID {
			if matched != "" {
				return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("ambiguous short repo_id %q matches multiple repos", repoID)}
			}
			matched = rec.RepoID
		}
	}
	if matched != "" {
		return matched, nil
	}
	return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("unknown repo_id: %s (run eng_list_repos)", repoID)}
}

// resolveRepoIDOrSingleton behaves like resolveRepoID, but when repoID is
// empty and exactly one repo is registered it returns that repo's id (no
// caller-side scoping needed). When repoID is empty and there are zero or
// many repos it returns an actionable InvalidParams (solov2-7tz1).
func resolveRepoIDOrSingleton(ctx context.Context, repos application.RepoLister, repoID string) (string, *RPCError) {
	if repoID != "" {
		return resolveRepoID(ctx, repos, repoID)
	}
	if repos == nil {
		return "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
	}
	all, err := repos.ListRepos(ctx)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
	}
	switch len(all) {
	case 0:
		return "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered — run `veska repo add <path>` first)"}
	case 1:
		return all[0].RepoID, nil
	default:
		return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo_id is required (%d repos registered; pass eng_list_repos to find the id)", len(all))}
	}
}

// resolveBranchOrActive returns branch when non-empty, otherwise the registered
// active_branch of repoID. Used so callers can omit `branch` when they are
// operating against the repo's current branch — overwhelmingly the common
// case (solov2-5vu1). Returns an InvalidParams when branch is empty and the
// repo's active_branch is also unset.
//
// repoID MUST already be resolved (full id, not a short prefix). When repos
// is nil the helper is a no-op pass-through (returns branch unchanged); the
// caller is responsible for checking emptiness in that mode.
func resolveBranchOrActive(ctx context.Context, repos application.RepoLister, repoID, branch string) (string, *RPCError) {
	if branch != "" {
		return branch, nil
	}
	if repos == nil {
		return "", &RPCError{Code: CodeInvalidParams, Message: "branch is required"}
	}
	all, err := repos.ListRepos(ctx)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
	}
	for _, rec := range all {
		if rec.RepoID == repoID {
			if rec.ActiveBranch == "" {
				return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("branch is required (repo %s has no recorded active_branch)", ShortRepoID(repoID))}
			}
			return rec.ActiveBranch, nil
		}
	}
	return "", &RPCError{Code: CodeInvalidParams, Message: "branch is required"}
}

// bindParams unmarshals raw into dst, returning an InvalidParams RPCError on failure.
func bindParams(raw json.RawMessage, dst any) *RPCError {
	if err := json.Unmarshal(raw, dst); err != nil {
		return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
	}
	return nil
}

// checkRequired returns an InvalidParams RPCError naming *every* empty value
// in the alternating name/value pairs, so a caller missing several params
// learns all of them from one round-trip instead of fixing them one error
// at a time (solov2-d2x). E.g. checkRequired("repo_id", p.RepoID, "branch", p.Branch).
func checkRequired(nameVal ...string) *RPCError {
	var missing []string
	for i := 0; i+1 < len(nameVal); i += 2 {
		if nameVal[i+1] == "" {
			missing = append(missing, nameVal[i])
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if len(missing) == 1 {
		return &RPCError{Code: CodeInvalidParams, Message: missing[0] + " is required"}
	}
	return &RPCError{Code: CodeInvalidParams, Message: "missing required params: " + strings.Join(missing, ", ")}
}
