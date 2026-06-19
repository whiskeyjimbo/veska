// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// minRepoIDPrefix is the minimum prefix length allowed when resolving a repository ID alias to prevent collisions.
const minRepoIDPrefix = 4

// defaultListLimit caps the top-level collection of unbounded list tools
// (clones, clusters, findings) so a first page fits an agent's context budget.
// Callers can override via an explicit "limit" param; total/truncated report
// the full result set so the agent knows more exists.
const defaultListLimit = 100

// clampListLimit returns the effective page size: the explicit limit when
// positive, otherwise the default cap.
func clampListLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	return limit
}

// normalizeDirection maps either accepted traversal vocabulary onto a single
// canonical form so all graph-traversal tools accept the same inputs as aliases.
// The semantics: in==callers (inbound, "who calls this"),
// out==callees (outbound, "what this calls"), both==both. An empty string is
// passed through neutrally (ok=true, "") so each tool can apply its OWN default
// downstream - collapsing empty into a canonical value here would silently flip
// blast's callers-default. Unrecognized values return ok=false so the caller
// still surfaces the existing CodeInvalidParams error rather than defaulting.
func normalizeDirection(s string) (canonical string, ok bool) {
	switch s {
	case "":
		return "", true
	case "in", "callers":
		return "in", true
	case "out", "callees":
		return "out", true
	case "both":
		return "both", true
	default:
		return "", false
	}
}

// resolveRepoID resolves a repository identifier alias, short ID, or prefix to its full canonical ID.
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
	// Step 2 match.
	for _, rec := range all {
		if ShortRepoID(rec.RepoID) == repoID {
			return rec.RepoID, nil
		}
	}
	// Step 3 match.
	for _, rec := range all {
		if slices.Contains(rec.Aliases, repoID) {
			return rec.RepoID, nil
		}
	}
	// Step 4 match.
	if len(repoID) >= minRepoIDPrefix {
		var matched string
		for _, rec := range all {
			if strings.HasPrefix(rec.RepoID, repoID) {
				if matched != "" {
					return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("ambiguous repo_id prefix %q matches multiple repos", repoID)}
				}
				matched = rec.RepoID
			}
		}
		if matched != "" {
			return matched, nil
		}
	}
	return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("unknown repo_id: %s (run eng_list_repos; prefixes must be >= %d chars)", repoID, minRepoIDPrefix)}
}

// resolveRepoIDOrSingleton returns the repository ID if provided, or the single registered repository if only one exists.
func resolveRepoIDOrSingleton(ctx context.Context, repos application.RepoLister, repoID string) (string, *RPCError) {
	return resolveRepoIDOrCwd(ctx, repos, repoID, "")
}

// resolveRepoIDOrCwd resolves the repository ID, falling back to a cwd-based repository match if the ID is omitted.
func resolveRepoIDOrCwd(ctx context.Context, repos application.RepoLister, repoID, cwd string) (string, *RPCError) {
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
		return "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered - run `veska repo add <path>` first)"}
	case 1:
		return all[0].RepoID, nil
	}

	if cwd != "" {
		for _, rec := range all {
			if rec.RootPath == "" {
				continue
			}
			if cwdMatchesRoot(cwd, rec.RootPath) {
				return rec.RepoID, nil
			}
		}
	}
	return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo_id is required (%d repos registered; pass eng_list_repos to find the id)", userVisibleRepoCount(all))}
}

// cwdMatchesRoot reports whether cwd equals rootPath or resides under it.
func cwdMatchesRoot(cwd, rootPath string) bool {
	return cwd == rootPath || strings.HasPrefix(cwd, rootPath+"/")
}

// userVisibleRepoCount returns the number of user-registered repositories, excluding external module dependencies.
func userVisibleRepoCount(all []application.RepoRecord) int {
	n := 0
	for _, r := range all {
		if strings.HasPrefix(r.RepoID, "ext:") {
			continue
		}
		n++
	}
	return n
}

// cwdFromParams extracts the "cwd" field from raw JSON parameters.
func cwdFromParams(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Cwd
}

// resolveRepoIDFromParams resolves a repository ID from explicit parameters, falling back to cwd.
func resolveRepoIDFromParams(ctx context.Context, repos application.RepoLister, raw json.RawMessage, requestedID string) (string, *RPCError) {
	if requestedID != "" {
		return resolveRepoID(ctx, repos, requestedID)
	}
	return resolveRepoIDOrCwd(ctx, repos, "", cwdFromParams(raw))
}

// repoBranch pairs a repository ID with a branch name.
type repoBranch struct {
	RepoID string
	Branch string
}

// resolveRepoFanoutFromParams identifies all target repositories and branches for a query, fanning out across all repositories when repo_id is omitted and cwd does not match.
func resolveRepoFanoutFromParams(ctx context.Context, repos application.RepoLister, raw json.RawMessage, requestedID, callerBranch string) (targets []repoBranch, fanout bool, rpcErr *RPCError) {
	if requestedID != "" {
		full, rerr := resolveRepoID(ctx, repos, requestedID)
		if rerr != nil {
			return nil, false, rerr
		}
		br, rerr := resolveBranchOrActive(ctx, repos, full, callerBranch)
		if rerr != nil {
			return nil, false, rerr
		}
		return []repoBranch{{RepoID: full, Branch: br}}, false, nil
	}
	if repos == nil {
		return nil, false, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
	}
	all, err := repos.ListRepos(ctx)
	if err != nil {
		return nil, false, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
	}
	if len(all) == 0 {
		return nil, false, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered - run `veska repo add <path>` first)"}
	}
	if len(all) == 1 {
		br := callerBranch
		if br == "" {
			br = all[0].ActiveBranch
		}
		if br == "" {
			return nil, false, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("branch is required (repo %s has no recorded active_branch)", ShortRepoID(all[0].RepoID))}
		}
		return []repoBranch{{RepoID: all[0].RepoID, Branch: br}}, false, nil
	}
	cwd := cwdFromParams(raw)
	if cwd != "" {
		for _, rec := range all {
			if rec.RootPath == "" {
				continue
			}
			if cwdMatchesRoot(cwd, rec.RootPath) {
				br := callerBranch
				if br == "" {
					br = rec.ActiveBranch
				}
				return []repoBranch{{RepoID: rec.RepoID, Branch: br}}, false, nil
			}
		}
	}
	// Multi-repo fanout: every registered repo on its own active_branch.
	// callerBranch is intentionally ignored here - a single branch name
	// can't sensibly span heterogenous repos.
	targets = make([]repoBranch, 0, len(all))
	for _, rec := range all {
		if rec.ActiveBranch == "" {
			continue
		}
		targets = append(targets, repoBranch{RepoID: rec.RepoID, Branch: rec.ActiveBranch})
	}
	if len(targets) == 0 {
		return nil, false, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo_id is required (%d repos registered; pass eng_list_repos to find the id)", userVisibleRepoCount(all))}
	}
	return targets, len(targets) > 1, nil
}

// fullNodeIDLen is the canonical length of a full node ID.
const fullNodeIDLen = 64

// expandNodeIDPrefix expands a short node ID prefix to its full canonical ID.
func expandNodeIDPrefix(ctx context.Context, graph ports.GraphReader, repoID, branch, nodeID string) (string, *RPCError) {
	if graph == nil || len(nodeID) == fullNodeIDLen {
		return nodeID, nil
	}
	g, err := graph.LoadGraph(ctx, repoID, branch)
	if err != nil {
		return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("expand node_id: load graph: %v", err)}
	}
	if g == nil {
		return nodeID, nil
	}
	var matched string
	for _, n := range g.Nodes() {
		id := string(n.ID)
		if !strings.HasPrefix(id, nodeID) {
			continue
		}
		if matched != "" {
			return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("node_id prefix %q is ambiguous; pass the full id from eng_find_symbol", nodeID)}
		}
		matched = id
	}
	if matched == "" {
		return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("node_id %q not in repo=%s branch=%s - the prefix may belong to a different registered repo; run `veska repo list` and retry with --repo <id> (or cd into the owning repo)", nodeID, ShortRepoID(repoID), branch)}
	}
	return matched, nil
}

// resolveSeedOwner identifies the repository and branch that owns a given query seed (node ID or symbol).
func resolveSeedOwner(ctx context.Context, repos application.RepoLister, graph ports.GraphReader, raw json.RawMessage, requestedRepoID, callerBranch, nodeID, symbol string) (repoID, branch, resolvedNodeID string, rpcErr *RPCError) {
	if nodeID == "" && symbol == "" {
		return "", "", "", &RPCError{Code: CodeInvalidParams, Message: "missing required params: node_id or symbol"}
	}

	resolveInRepo := func(repoID, branch string) (string, *RPCError) {
		if nodeID != "" {
			return expandNodeIDPrefix(ctx, graph, repoID, branch, nodeID)
		}
		if graph == nil {
			return "", &RPCError{Code: CodeInternalError, Message: "symbol lookup not wired (graph storage missing)"}
		}
		matches, err := graph.FindNodes(ctx, repoID, branch, symbol)
		if err != nil {
			return "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("find symbol %q: %v", symbol, err)}
		}
		if len(matches) == 0 {
			return "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("symbol not found: %s", symbol)}
		}
		if len(matches) > 1 {
			return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("symbol %q is ambiguous (%d matches); pass node_id to disambiguate", symbol, len(matches))}
		}
		return string(matches[0].ID), nil
	}

	// Path 1: explicit repo_id.
	if requestedRepoID != "" {
		full, rerr := resolveRepoID(ctx, repos, requestedRepoID)
		if rerr != nil {
			return "", "", "", rerr
		}
		br, rerr := resolveBranchOrActive(ctx, repos, full, callerBranch)
		if rerr != nil {
			return "", "", "", rerr
		}
		nid, rerr := resolveInRepo(full, br)
		if rerr != nil {
			return "", "", "", rerr
		}
		return full, br, nid, nil
	}

	if repos == nil {
		return "", "", "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required"}
	}
	all, err := repos.ListRepos(ctx)
	if err != nil {
		return "", "", "", &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list repos failed: %v", err)}
	}
	if len(all) == 0 {
		return "", "", "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered - run `veska repo add <path>` first)"}
	}
	if len(all) == 1 {
		br := callerBranch
		if br == "" {
			br = all[0].ActiveBranch
		}
		if br == "" {
			return "", "", "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("branch is required (repo %s has no recorded active_branch)", ShortRepoID(all[0].RepoID))}
		}
		nid, rerr := resolveInRepo(all[0].RepoID, br)
		if rerr != nil {
			return "", "", "", rerr
		}
		return all[0].RepoID, br, nid, nil
	}

	if cwd := cwdFromParams(raw); cwd != "" {
		for _, rec := range all {
			if rec.RootPath == "" {
				continue
			}
			if cwdMatchesRoot(cwd, rec.RootPath) {
				br := callerBranch
				if br == "" {
					br = rec.ActiveBranch
				}
				nid, rerr := resolveInRepo(rec.RepoID, br)
				if rerr == nil {
					return rec.RepoID, br, nid, nil
				}

				if rerr.Code != CodeNotFound {
					return "", "", "", rerr
				}
				break
			}
		}
	}

	// Path 3: fan-out by seed. Walk every repo; the seed's owner is the only
	// repo where lookup succeeds. The ambiguous and not-found cases produce
	// actionable errors that name the specific candidates so the caller can
	// retry with --repo.
	type hit struct {
		repoID, branch, nodeID string
	}
	var hits []hit
	for _, rec := range all {
		br := callerBranch
		if br == "" {
			br = rec.ActiveBranch
		}
		if br == "" {
			continue
		}
		if nodeID != "" {
			expanded, expErr := expandNodeIDPrefix(ctx, graph, rec.RepoID, br, nodeID)
			if expErr != nil || expanded == "" {
				continue
			}
			n, gerr := graph.GetNode(ctx, rec.RepoID, br, domain.NodeID(expanded))
			if gerr != nil || n == nil {
				continue
			}
			hits = append(hits, hit{repoID: rec.RepoID, branch: br, nodeID: expanded})
			continue
		}
		matches, gerr := graph.FindNodes(ctx, rec.RepoID, br, symbol)
		if gerr != nil || len(matches) == 0 {
			continue
		}
		if len(matches) > 1 {
			return "", "", "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("symbol %q is ambiguous in repo %s (%d matches); pass node_id to disambiguate", symbol, ShortRepoID(rec.RepoID), len(matches))}
		}
		hits = append(hits, hit{repoID: rec.RepoID, branch: br, nodeID: string(matches[0].ID)})
	}
	switch len(hits) {
	case 0:
		seed := nodeID
		if seed == "" {
			seed = symbol
		}
		return "", "", "", &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("seed %q not found in any of %d registered repos", seed, len(all))}
	case 1:
		return hits[0].repoID, hits[0].branch, hits[0].nodeID, nil
	default:
		shorts := make([]string, 0, len(hits))
		for _, h := range hits {
			shorts = append(shorts, ShortRepoID(h.repoID))
		}
		seed := nodeID
		if seed == "" {
			seed = symbol
		}
		return "", "", "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("seed %q exists in multiple repos (%s); pass --repo to disambiguate", seed, strings.Join(shorts, ", "))}
	}
}
