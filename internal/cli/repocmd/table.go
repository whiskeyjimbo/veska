package repocmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// formatScanElapsed renders ScanState.StartedAt → "1m23s" / "12s". A zero
// StartedAt yields "" so older daemons (no started_at in scans_in_flight)
// quietly omit the suffix.
func formatScanElapsed(startedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	d := time.Since(startedAt)
	if d < 0 {
		return ""
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%ds", mins, secs)
}

// PrintRepoTable renders the repo list as REPO_ID + ROOT + BRANCH + STATUS.
// A short repo_id (first 12 chars) is shown so the column is readable; the
// full id is still present in any tool output, and `veska repo remove`
// accepts the full id.
func PrintRepoTable(w io.Writer, repos []RepoView) {
	PrintRepoTableWithProgress(w, repos, nil)
}

// PrintRepoTableWithProgress overlays in-flight scan progress onto the
// (unindexed) rows so a user watching a long cold scan can tell hung
// from progressing (solov2-u9h9). progress maps repo_id → phase + files_seen.
func PrintRepoTableWithProgress(w io.Writer, repos []RepoView, progress map[string]ScanProgressRow) {
	if len(repos) == 0 {
		fmt.Fprintln(w, "no repositories registered — run: veska repo add <path>")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO_ID\tKIND\tALIAS\tBRANCH\tSTATUS\tROOT")
	for _, r := range repos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ShortRepoID(r.RepoID), repoKind(r), repoAlias(r),
			repoBranch(r), repoStatus(r, progress), r.RootPath)
	}
	_ = tw.Flush()
}

func repoBranch(r RepoView) string {
	if r.ActiveBranch == "" {
		return "-"
	}
	return r.ActiveBranch
}

func repoKind(r RepoView) string {
	if r.Kind == "" {
		return "tracked"
	}
	return r.Kind
}

func repoAlias(r RepoView) string {
	if len(r.Aliases) == 0 {
		return "-"
	}
	return strings.Join(r.Aliases, ",")
}

// repoStatus computes the STATUS cell for a row, overlaying scan progress and
// failure/missing signals onto the base promoted/(unindexed) state.
func repoStatus(r RepoView, progress map[string]ScanProgressRow) string {
	status := "promoted"
	if r.LastPromotedSHA == "" {
		status = unindexedStatus(r, progress)
	}
	// Flag repos whose root path no longer exists on disk so users can see
	// stale registrations at a glance (solov2-76px). `repo remove <id>` is
	// still the cleanup path.
	if r.RootPath != "" {
		if _, err := os.Stat(r.RootPath); errors.Is(err, fs.ErrNotExist) {
			status = "(missing)"
		}
	}
	return status
}

// unindexedStatus resolves the STATUS for a never-promoted repo: an in-flight
// scan's phase + file count, a tailed cold-scan failure, or "(unindexed)".
func unindexedStatus(r RepoView, progress map[string]ScanProgressRow) string {
	status := "(unindexed)"
	// solov2-jtl5.8: a never-promoted repo isn't always 'just hasn't
	// scanned yet'. A failed cold-scan leaves the repo in this state
	// too, and the user has no signal until they tail daemon.log. If
	// the most recent ERROR/WARN line in the log names this repo, surface
	// 'scan failed' instead of the silently-misleading '(unindexed)'.
	if _, inFlight := progress[r.RepoID]; !inFlight {
		if reason := tailScanFailureReason(daemonLogPath(), r.RepoID); reason != "" {
			status = "(scan failed)"
		}
	}
	p, ok := progress[r.RepoID]
	if !ok {
		return status
	}
	status = scanPhaseStatus(p, status)
	// solov2-jtl5.1: append elapsed so a user can tell a slow-but-
	// progressing scan from a hung one even when files_seen plateaus
	// on a single large file. Older daemons omit started_at and the
	// suffix is suppressed.
	if elapsed := formatScanElapsed(p.StartedAt); elapsed != "" && status != "(unindexed)" {
		status = status[:len(status)-1] + ", " + elapsed + ")"
	}
	return status
}

// scanPhaseStatus maps an in-flight scan's phase + files_seen to the status
// cell, falling back to base when the phase is empty and no files seen yet.
func scanPhaseStatus(p ScanProgressRow, base string) string {
	switch {
	case p.Phase == "promoting":
		return fmt.Sprintf("(promoting, %d files)", p.FilesSeen)
	case p.Phase == "walking" && p.FilesSeen > 0:
		return fmt.Sprintf("(walking, %d files)", p.FilesSeen)
	case p.Phase != "":
		return fmt.Sprintf("(%s)", p.Phase)
	case p.FilesSeen > 0:
		return fmt.Sprintf("(scanning, %d files)", p.FilesSeen)
	}
	return base
}

// ColdScanRunningHint returns the post-`repo add` hint shown when the daemon
// has accepted a new repo and is cold-scanning it asynchronously. The hint
// must name `veska repo add <path> --wait` explicitly — `--wait` is a flag on
// `repo add`, not on `repo list`, and a copy-pasteable suggestion avoids the
// solov2-rhaq trap where juniors run `veska repo list --wait` and hit
// "unknown flag".
func ColdScanRunningHint(root, logPath string) string {
	return fmt.Sprintf("  cold scan running in the background — `veska repo list` shows status; re-run with `veska repo add %s --wait` to block until it finishes, or `tail %s` for live progress", root, logPath)
}
