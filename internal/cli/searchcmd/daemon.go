// SPDX-License-Identifier: AGPL-3.0-only

package searchcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// SearchHeaderMode discriminates the three scope-resolution paths the
// 'searching:.' stderr header announces.
type SearchHeaderMode int

const (
	// SearchHeaderModeCwd: empty target, cwd resolved to a registered repo.
	// The header includes a "(use --repo to override)" hint so the user
	// knows fan-out is one flag away.
	SearchHeaderModeCwd SearchHeaderMode = iota
	// SearchHeaderModeExplicit: --repo (or positional target) selected a
	// specific repo. The user already named it; no override hint needed.
	SearchHeaderModeExplicit
	// SearchHeaderModeAll: no target, cwd outside any registered repo
	// daemon fan-out across every registered repo.
	SearchHeaderModeAll
)

// SearchHeaderInfo carries the data needed to render a 'searching:' header.
type SearchHeaderInfo struct {
	Mode    SearchHeaderMode
	RepoID  string
	ShortID string
	Aliases []string
}

// EmitSearchHeader writes a one-line 'searching: <label>' notice to stderr
// announcing the repo scope. JSON mode suppresses the header entirely so
// `--json` output stays a clean machine-consumable envelope.
func EmitSearchHeader(stderr, stdout io.Writer, jsonOut bool, info SearchHeaderInfo) {
	_ = stdout // explicit reminder: header NEVER goes to stdout
	if jsonOut {
		return
	}
	if stderr == nil {
		return
	}
	switch info.Mode {
	case SearchHeaderModeAll:
		fmt.Fprintln(stderr, "searching: all repos")
	case SearchHeaderModeCwd:
		label := repoDisplayLabel(info)
		fmt.Fprintf(stderr, "searching: %s (use --repo to override)\n", label)
	case SearchHeaderModeExplicit:
		label := repoDisplayLabel(info)
		fmt.Fprintf(stderr, "searching: %s\n", label)
	}
}

// repoDisplayLabel picks the most useful human-readable identifier for a repo
// header line: first alias if any, else short_id, else a 12-char prefix of
// repo_id.
func repoDisplayLabel(info SearchHeaderInfo) string {
	if len(info.Aliases) > 0 && info.Aliases[0] != "" {
		return info.Aliases[0]
	}
	if info.ShortID != "" {
		return info.ShortID
	}
	if len(info.RepoID) > 12 {
		return info.RepoID[:12]
	}
	return info.RepoID
}

// daemonSearch resolves the target repo through a running daemon and runs the
// query via eng_search_semantic. handled is false when the daemon is
// unreachable or the target repo is not yet tracked - the caller then falls
// back to the in-process clone/index/query path. This keeps the common
// "search my already-indexed repo" case on the daemon's hybrid pipeline and
// avoids a second writer on veska.db ().
func daemonSearch(ctx context.Context, stderr, stdout io.Writer, opts RunOpts) (SearchEnvelope, bool, error) {
	// A git-URL target needs a clone+index pass the daemon-dial path does
	// not perform; leave it to the in-process path.
	if isGitURL(opts.Target) {
		return SearchEnvelope{}, false, nil
	}

	repoID, branch, info, ok := resolveRepoViaDaemonInfo(ctx, opts.Target)
	if !ok {
		// when the cwd isn't part of any registered repo
		// (junior who registered repos in another dir and ran search from
		// /tmp), fan out across every registered repo instead of erroring.
		// Only fires when target is empty - explicit paths / URLs still go
		// through the in-process path so we don't change their semantics.
		if opts.Target == "" {
			env, fanned, ferr := daemonSearchAllRepos(ctx, stderr, stdout, opts)
			if fanned {
				return env, true, ferr
			}
		}
		return SearchEnvelope{}, false, nil
	}

	// announce which repo we're searching so cwd-scoping
	// isn't silent. cwd-mode adds a '--repo to override' hint; explicit-mode
	// (user passed --repo / positional) skips the hint.
	mode := SearchHeaderModeCwd
	if opts.Target != "" {
		mode = SearchHeaderModeExplicit
	}
	info.Mode = mode
	EmitSearchHeader(stderr, stdout, opts.JSONOut, info)

	var env SearchEnvelope
	if err := mcpclient.Call(ctx, "eng_search_semantic", map[string]any{
		"repo_id": repoID,
		"branch":  branch,
		"query":   opts.Query,
		"k":       kOrDefault(opts.K),
	}, &env); err != nil {
		// Daemon was reachable enough to resolve the repo but the search
		// call failed - surface it rather than silently re-running a
		// divergent in-process query.
		return SearchEnvelope{}, true, fmt.Errorf("search: daemon eng_search_semantic: %w", err)
	}
	return env, true, nil
}

// daemonSearchByRepoID runs eng_search_semantic against a known repo_id/branch.
// Returned ok is false when the daemon is unreachable so callers can fall back
// to the in-process search service. Used by the URL/path path of Run after
// ensureIndexed has registered the repo.
func daemonSearchByRepoID(ctx context.Context, repoID, branch string, opts RunOpts) (SearchEnvelope, bool, error) {
	var env SearchEnvelope
	if err := mcpclient.Call(ctx, "eng_search_semantic", map[string]any{
		"repo_id": repoID,
		"branch":  branchOrMain(branch),
		"query":   opts.Query,
		"k":       kOrDefault(opts.K),
	}, &env); err != nil {
		// Distinguish "daemon down" (fall back) from "daemon up but call
		// failed" (surface). mcpclient returns connection errors which we
		// treat as unreachable; anything else is a real search failure.
		if mcpclient.IsDaemonUnreachable(err) {
			return SearchEnvelope{}, false, nil
		}
		return SearchEnvelope{}, true, fmt.Errorf("search: daemon eng_search_semantic: %w", err)
	}
	return env, true, nil
}

// daemonSearchAllRepos is the cross-repo fanout invoked when target is empty
// and cwd is not part of a registered repo. It lists every
// registered repo, runs eng_search_semantic per repo with the same k, merges
// results, re-sorts by score desc, and trims to k. fanned is false when the
// registry is empty so the caller surfaces the existing "not registered" error
// instead of a silent zero-result success.
func daemonSearchAllRepos(ctx context.Context, stderr, stdout io.Writer, opts RunOpts) (SearchEnvelope, bool, error) {
	type repoRow struct {
		RepoID       string `json:"repo_id"`
		ActiveBranch string `json:"active_branch"`
	}
	var lr struct {
		Repos []repoRow `json:"repos"`
	}
	if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
		return SearchEnvelope{}, false, nil
	}
	if len(lr.Repos) == 0 {
		return SearchEnvelope{}, false, nil
	}
	// emit the 'searching: all repos' header before the
	// fanout fires so the user knows we did NOT scope to a single repo.
	EmitSearchHeader(stderr, stdout, opts.JSONOut, SearchHeaderInfo{Mode: SearchHeaderModeAll})
	k := kOrDefault(opts.K)
	var merged SearchEnvelope
	for _, r := range lr.Repos {
		branch := branchOrMain(r.ActiveBranch)
		var env SearchEnvelope
		if err := mcpclient.Call(ctx, "eng_search_semantic", map[string]any{
			"repo_id": r.RepoID,
			"branch":  branch,
			"query":   opts.Query,
			"k":       k,
		}, &env); err != nil {
			// Per-repo failures must not abort the whole fanout - a stuck
			// repo would otherwise suppress every other repo's hits. Track
			// it in degraded_reasons so the user still sees something.
			merged.DegradedReasons = append(merged.DegradedReasons, fmt.Sprintf("repo %s search failed: %v", r.RepoID, err))
			continue
		}
		for _, h := range env.Results {
			h.RepoID = r.RepoID
			merged.Results = append(merged.Results, h)
		}
		merged.DegradedReasons = append(merged.DegradedReasons, env.DegradedReasons...)
	}
	// Score-desc sort across the combined set, then trim to k. The score is
	// the daemon's post-fusion RRF - same scale across repos because the
	// embedder is one process - so cross-repo comparison is sound.
	sort.SliceStable(merged.Results, func(i, j int) bool {
		return merged.Results[i].Score > merged.Results[j].Score
	})
	if len(merged.Results) > k {
		merged.Results = merged.Results[:k]
	}
	return merged, true, nil
}

// daemonRepoRow is the registry row shape resolveRepoViaDaemonInfo matches
// against, with the display fields the 'searching:' header needs.
type daemonRepoRow struct {
	RepoID       string   `json:"repo_id"`
	ShortID      string   `json:"short_id"`
	Aliases      []string `json:"aliases"`
	RootPath     string   `json:"root_path"`
	ActiveBranch string   `json:"active_branch"`
}

func (r daemonRepoRow) headerInfo() SearchHeaderInfo {
	return SearchHeaderInfo{
		RepoID:  r.RepoID,
		ShortID: r.ShortID,
		Aliases: r.Aliases,
	}
}

// resolveRepoViaDaemonInfo maps the search target to a (repo_id, branch) the
// daemon already tracks, plus the display info (short_id, aliases) needed for
// the 'searching:' header. Resolution order:
//
//	empty target → eng_get_current_repo against cwd
//	non-empty → match against eng_list_repos by full repo_id, short_id,
//	  or alias first (cheap), then by canonical filesystem root.
//
// ok is false (caller falls back to the in-process path) when the daemon is
// down or the repo is unknown.
func resolveRepoViaDaemonInfo(ctx context.Context, target string) (repoID, branch string, info SearchHeaderInfo, ok bool) {
	if target == "" {
		return resolveCurrentRepoViaDaemon(ctx)
	}
	return resolveTargetRepoViaDaemon(ctx, target)
}

// resolveCurrentRepoViaDaemon resolves cwd to a tracked repo via
// eng_get_current_repo.
func resolveCurrentRepoViaDaemon(ctx context.Context) (repoID, branch string, info SearchHeaderInfo, ok bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", SearchHeaderInfo{}, false
	}
	var res struct {
		Repo daemonRepoRow `json:"repo"`
	}
	if err := mcpclient.Call(ctx, "eng_get_current_repo", map[string]any{"cwd": cwd}, &res); err != nil {
		return "", "", SearchHeaderInfo{}, false
	}
	if res.Repo.RepoID == "" {
		return "", "", SearchHeaderInfo{}, false
	}
	return res.Repo.RepoID, branchOrMain(res.Repo.ActiveBranch), res.Repo.headerInfo(), true
}

// resolveTargetRepoViaDaemon matches an explicit target against eng_list_repos
// by registry identifier (repo_id, short_id, alias) then by filesystem root.
func resolveTargetRepoViaDaemon(ctx context.Context, target string) (repoID, branch string, info SearchHeaderInfo, ok bool) {
	var list struct {
		Repos []daemonRepoRow `json:"repos"`
	}
	if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &list); err != nil {
		return "", "", SearchHeaderInfo{}, false
	}

	// Registry-identifier match: full repo_id, short_id, or any bound alias.
	// Path matching can't usefully resolve any of these, so a hit here saves
	// a filesystem stat round and works for non-path targets like "lib".
	for _, r := range list.Repos {
		if target == r.RepoID || target == r.ShortID || slices.Contains(r.Aliases, target) {
			return r.RepoID, branchOrMain(r.ActiveBranch), r.headerInfo(), true
		}
	}

	// Filesystem-root match: target was a path. filepath.Abs turns "lib"
	// into "$cwd/lib" which wouldn't match anything in the registry - but
	// that's fine because we already exhausted the identifier-string match
	// above and a literal alias would never coincide with a real path.
	canonical, err := filepath.Abs(target)
	if err != nil {
		return "", "", SearchHeaderInfo{}, false
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}
	for _, r := range list.Repos {
		if r.RootPath == canonical {
			return r.RepoID, branchOrMain(r.ActiveBranch), r.headerInfo(), true
		}
	}
	return "", "", SearchHeaderInfo{}, false
}

// pendingEmbedsHint asks the daemon (if reachable) how many embeds are still
// queued so a zero-result search can tell the user "the index is still warming
// up" instead of staying silent. Returns ok=false if the daemon is down or
// doesn't expose the field - the caller falls back to a plain "no results"
// line.
func pendingEmbedsHint() (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var status struct {
		PendingEmbeds int `json:"pending_embeds"`
	}
	if err := mcpclient.Call(ctx, "eng_get_status", map[string]any{}, &status); err != nil {
		return 0, false
	}
	return status.PendingEmbeds, true
}

// kOrDefault defaults a non-positive k to 10.
func kOrDefault(k int) int {
	if k <= 0 {
		return 10
	}
	return k
}
