// SPDX-License-Identifier: AGPL-3.0-only

package doctor

// RepoFreshnessRef is the minimal per-repo input CheckIndexFreshness needs.
// Defined here (rather than importing the repo registry record) so the doctor
// package stays unidirectional and the check is testable with plain structs.
type RepoFreshnessRef struct {
	RepoID      string
	Branch      string
	RootPath    string
	PromotedSHA string
}

// HeadResolver returns the current git HEAD sha for a repo root. Injected so
// the check is a pure function over its inputs (mirrors the pendingCounter
// pattern in embedding_backlog.go); production callers pass git.Querier.HEAD.
type HeadResolver func(rootPath string) (string, error)

// RepoFreshness is the per-repo freshness verdict. State values:
//
//	"current"        - HEAD == last promoted sha. NOTE: a deliberately WEAK
//	                   signal. Veska indexes the working tree, not a commit, so
//	                   a matching sha only means no commit has landed since the
//	                   last promotion - it does NOT guarantee the parse reflects
//	                   the files on disk (uncommitted edits, or edits made while
//	                   no watcher observed them, can still be unindexed).
//	"behind"         - HEAD != last promoted sha: a commit landed since the last
//	                   promotion. Actionable - run `veska reindex`.
//	"never_promoted" - repo registered but never indexed (no promoted sha yet).
//	"unknown"        - git HEAD could not be resolved (not a git repo, git
//	                   missing, etc.).
type RepoFreshness struct {
	RepoID      string `json:"repo_id"`
	Branch      string `json:"branch"`
	PromotedSHA string `json:"promoted_sha"`
	HeadSHA     string `json:"head_sha,omitempty"`
	State       string `json:"state"`
}

// IndexFreshnessReport summarizes whether each registered repo's promoted sha
// matches its current git HEAD. It is INFORMATIONAL: a "behind" repo must NOT
// promote the doctor rollup. Veska auto-reconciles via fsnotify and the startup
// resync, so a just-committed-but-not-yet-reindexed mismatch is a normal,
// transient state; promoting it to "degraded" on every un-reindexed commit
// would cry wolf. Status is "behind" when any repo is behind, else "current".
type IndexFreshnessReport struct {
	Repos  []RepoFreshness `json:"repos"`
	Status string          `json:"status"`
}

// CheckIndexFreshness compares each repo's last promoted sha against its current
// git HEAD, using the injected resolver. Pure given the resolver.
func CheckIndexFreshness(repos []RepoFreshnessRef, head HeadResolver) IndexFreshnessReport {
	rep := IndexFreshnessReport{Status: "current"}
	for _, r := range repos {
		rf := RepoFreshness{RepoID: r.RepoID, Branch: r.Branch, PromotedSHA: r.PromotedSHA}
		switch r.PromotedSHA {
		case "":
			rf.State = "never_promoted"
		default:
			h, err := head(r.RootPath)
			switch {
			case err != nil || h == "":
				rf.State = "unknown"
			case h == r.PromotedSHA:
				rf.State = "current"
			default:
				rf.HeadSHA = h
				rf.State = "behind"
				rep.Status = "behind"
			}
		}
		rep.Repos = append(rep.Repos, rf)
	}
	return rep
}

// BehindCount returns how many repos are in the "behind" state.
func (r IndexFreshnessReport) BehindCount() int {
	n := 0
	for _, rf := range r.Repos {
		if rf.State == "behind" {
			n++
		}
	}
	return n
}
