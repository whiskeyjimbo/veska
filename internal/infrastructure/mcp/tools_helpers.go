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

// minRepoIDPrefix is the shortest prefix accepted as a repo_id alias. Below
// this length almost any input would match every registered repo by chance;
// keeping it >= 4 means callers either pass the full id, a deliberate
// short_id, or a long-enough prefix to be meaningful (solov2-rkbc).
const minRepoIDPrefix = 4

// resolveRepoID validates that repoID names a repo the daemon tracks and
// returns its canonical full id. The match progression is:
//
//  1. exact full-id match
//  2. exact short_id (ShortRepoIDLen chars) match
//  3. unambiguous prefix (>= minRepoIDPrefix chars) of any registered full id
//
// Step 3 honours the README contract that "anywhere a repo_id is required
// you may pass the full id or that short prefix" (solov2-rkbc) — previously
// only step 2 worked, and an 8-char prefix returned NotFound. When no repo
// matches, a NotFound RPCError is returned so a stale or mistyped id
// surfaces as a loud error instead of a silently-empty result (solov2-5rh).
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
	// Step 2: exact ShortRepoIDLen-char short_id match.
	for _, rec := range all {
		if ShortRepoID(rec.RepoID) == repoID {
			return rec.RepoID, nil
		}
	}
	// Step 3: user-set alias (solov2-7w1t). Beats prefix so an explicit
	// alias never gets shadowed by a colliding hex prefix.
	for _, rec := range all {
		if slices.Contains(rec.Aliases, repoID) {
			return rec.RepoID, nil
		}
	}
	// Step 4: unambiguous prefix of any full id, minRepoIDPrefix chars or longer.
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

// resolveRepoIDOrSingleton behaves like resolveRepoID, but when repoID is
// empty and exactly one repo is registered it returns that repo's id (no
// caller-side scoping needed). When repoID is empty and there are zero or
// many repos it returns an actionable InvalidParams (solov2-7tz1).
func resolveRepoIDOrSingleton(ctx context.Context, repos application.RepoLister, repoID string) (string, *RPCError) {
	return resolveRepoIDOrCwd(ctx, repos, repoID, "")
}

// resolveRepoIDOrCwd extends resolveRepoIDOrSingleton with a third
// fallback: when repoID is empty AND multiple repos are registered, if cwd
// matches a registered repo's RootPath (or sits inside one), return that
// repo. This bridges the gap for callers running inside a registered repo
// who would otherwise have to look up a short_id even though the daemon
// has the cwd context (solov2-ktz0).
//
// Callers extract cwd from their params struct (a `cwd` field injected by
// the veska-mcp shim — see cmd/veska-mcp/cwd_inject.go) and pass it in.
// Empty cwd reproduces the singleton-only behaviour.
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
		return "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered — run `veska repo add <path>` first)"}
	case 1:
		return all[0].RepoID, nil
	}
	// Multi-repo: try cwd before erroring. We accept either exact RootPath
	// equality OR cwd sitting inside a registered RootPath, so a call from
	// a subdirectory of the repo resolves the same as a call from the root.
	if cwd != "" {
		for _, rec := range all {
			if rec.RootPath == "" {
				continue
			}
			if cwd == rec.RootPath || strings.HasPrefix(cwd, rec.RootPath+"/") {
				return rec.RepoID, nil
			}
		}
	}
	return "", &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo_id is required (%d repos registered; pass eng_list_repos to find the id)", len(all))}
}

// cwdFromParams unmarshals just the "cwd" field from a raw JSON-RPC params
// blob. Used by query tools to pick up the cwd hint injected by the
// veska-mcp shim without adding a `cwd` field to every params struct
// (solov2-ktz0). Returns "" when the field is missing, blank, or the blob
// is malformed — none of which should fail the caller, since cwd is a
// best-effort hint.
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

// resolveRepoIDFromParams is the convenience used by repo-scoped query tools
// (solov2-ktz0): if requestedID is non-empty resolve it normally; otherwise
// fall back to the cwd hint extracted from raw. Tools call this in place of
// the older `checkRequired("repo_id", ...) + resolveRepoID(...)` pair so
// requests omitting repo_id resolve from the caller's cwd when possible.
func resolveRepoIDFromParams(ctx context.Context, repos application.RepoLister, raw json.RawMessage, requestedID string) (string, *RPCError) {
	if requestedID != "" {
		return resolveRepoID(ctx, repos, requestedID)
	}
	return resolveRepoIDOrCwd(ctx, repos, "", cwdFromParams(raw))
}

// repoBranch pairs a resolved repo_id with the branch the caller should
// query on it. Used by the fanout helpers when the same query needs to run
// across multiple repos with each repo's own active_branch.
type repoBranch struct {
	RepoID string
	Branch string
}

// resolveRepoFanoutFromParams returns the set of (repo_id, branch) targets a
// query tool should hit (solov2-g8fh). It generalises resolveRepoIDFromParams:
//
//   - requestedID non-empty: single target, branch defaults to that repo's
//     active_branch when caller-supplied branch is empty.
//   - empty registry: InvalidParams (same message as the singleton helper).
//   - exactly one repo registered: single target on that repo.
//   - cwd matches a registered RootPath (or is inside one): single target
//     on the matched repo — caller is operating "inside" that repo.
//   - multiple repos AND no cwd match: fanout across every registered repo,
//     each using its own active_branch. This is the new behaviour — the
//     previous error path ("repo_id is required (N repos registered…)") was
//     a junior-hostile dead end when calling eng_find_symbol / semantic
//     search from a shell that wasn't `cd`'d into any registered repo.
//
// callerBranch is the explicit `branch` field from the params; it only
// applies to the requestedID path (a single branch can't sensibly span
// repos). For the fanout case each target gets its own ActiveBranch.
//
// fanout reports whether the result contains more than one target so the
// caller can populate per-hit repo_id only when disambiguation matters.
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
		return nil, false, &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered — run `veska repo add <path>` first)"}
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
			if cwd == rec.RootPath || strings.HasPrefix(cwd, rec.RootPath+"/") {
				br := callerBranch
				if br == "" {
					br = rec.ActiveBranch
				}
				return []repoBranch{{RepoID: rec.RepoID, Branch: br}}, false, nil
			}
		}
	}
	// Multi-repo fanout: every registered repo on its own active_branch.
	// callerBranch is intentionally ignored here — a single branch name
	// can't sensibly span heterogenous repos.
	targets = make([]repoBranch, 0, len(all))
	for _, rec := range all {
		if rec.ActiveBranch == "" {
			continue
		}
		targets = append(targets, repoBranch{RepoID: rec.RepoID, Branch: rec.ActiveBranch})
	}
	if len(targets) == 0 {
		return nil, false, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("repo_id is required (%d repos registered; pass eng_list_repos to find the id)", len(all))}
	}
	return targets, len(targets) > 1, nil
}

// resolveSeedOwner picks the (repo_id, branch, node_id) triple for a seeded
// graph query (eng_get_call_chain, eng_get_blast_radius) when the caller may
// omit repo_id. It honours the same documented contract as the --repo flag's
// help text: "default: fan out across registered repos".
//
// Resolution order:
//
//  1. requestedRepoID given → resolve it; if symbol given resolve to node_id
//     inside that repo.
//  2. cwd injected via params → if it matches a registered RootPath, pin
//     to that repo (same path as resolveRepoIDOrCwd).
//  3. multi-repo fan-out by seed: walk every registered repo and look up the
//     seed in each. Exactly one owner → use it. Zero → NotFound. Multiple →
//     ambiguous, ask the caller to pin --repo.
//
// nodeID wins when both seeds are supplied (it is globally unique by
// construction). graph may be nil only when repos is also nil (composition
// roots without persistence wired) — fan-out then degrades to "no match".
func resolveSeedOwner(ctx context.Context, repos application.RepoLister, graph ports.GraphStorage, raw json.RawMessage, requestedRepoID, callerBranch, nodeID, symbol string) (repoID, branch, resolvedNodeID string, rpcErr *RPCError) {
	if nodeID == "" && symbol == "" {
		return "", "", "", &RPCError{Code: CodeInvalidParams, Message: "missing required params: node_id or symbol"}
	}

	// resolveInRepo turns the seed into a node_id within (repoID, branch).
	// For an explicit node_id we trust the caller — the downstream BFS
	// (call-chain / blast) surfaces the empty result if the node is absent,
	// matching the pre-fanout contract. For a symbol we still validate so
	// the caller learns about ambiguity / typos up front.
	resolveInRepo := func(repoID, branch string) (string, *RPCError) {
		if nodeID != "" {
			return nodeID, nil
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
		return "", "", "", &RPCError{Code: CodeInvalidParams, Message: "repo_id is required (no repos registered — run `veska repo add <path>` first)"}
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

	// Path 2: cwd pin.
	if cwd := cwdFromParams(raw); cwd != "" {
		for _, rec := range all {
			if rec.RootPath == "" {
				continue
			}
			if cwd == rec.RootPath || strings.HasPrefix(cwd, rec.RootPath+"/") {
				br := callerBranch
				if br == "" {
					br = rec.ActiveBranch
				}
				nid, rerr := resolveInRepo(rec.RepoID, br)
				if rerr != nil {
					return "", "", "", rerr
				}
				return rec.RepoID, br, nid, nil
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
			n, gerr := graph.GetNode(ctx, rec.RepoID, br, domain.NodeID(nodeID))
			if gerr != nil || n == nil {
				continue
			}
			hits = append(hits, hit{repoID: rec.RepoID, branch: br, nodeID: nodeID})
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
