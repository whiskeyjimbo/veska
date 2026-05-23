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
