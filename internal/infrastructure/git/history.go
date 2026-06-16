// history.go exposes read-only commit-history queries — ChangeCounts and
// FileHistory — thin os/exec wrappers around `git log` used by the
// hot_zone ranking surface and the eng_get_context_pack MCP tool to
// derive per-file recent-change frequency.
// git log is the data source (rather than post_promotion_queue done-rows,
// which may be gc-pruned). The full log is walked and windowed in-process
// on each commit's date; results are sorted so a fixed repo state + window
// yields deterministic output.
// File paths are repoRoot-relative — the same convention as diff.go.

package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultWindow is the look-back applied when ChangeCounts / FileHistory
// are called with a zero window.
const defaultWindow = 30 * 24 * time.Hour

// Commit is a single commit touching a queried file path. Fields are the
// minimum the hot_zone surface and context-pack tool need; the full diff
// is intentionally not exposed (read-only history, not blast radius).
type Commit struct {
	Hash    string    // full commit hash
	Author  string    // author name
	When    time.Time // author date
	Subject string    // commit subject line
}

// ChangeCounts returns per-file change counts: for every file touched by
// a commit within window, the number of commits in that window that
// modified it. A zero window selects the 30-day default. Out-of-window
// commits are excluded. The map is keyed by repoRoot-relative path.
// Filtering is done in-process on each commit's date rather than via
// `git log --since`: --since stops history traversal at the first
// out-of-window commit, so an out-of-window HEAD would hide every
// in-window ancestor. Walking the full log and filtering ourselves
// is correct regardless of commit-date ordering.
// An empty repoRoot returns an error rather than shelling out against
// the process cwd: history queries must always run scoped to a repo.
func ChangeCounts(ctx context.Context, repoRoot string, window time.Duration) (map[string]int, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git log: repoRoot is empty")
	}
	cutoff := cutoffTime(window)
	// %x1f-delimited header line, then --name-only paths, then a blank
	// line. A leading record-separator marks header lines apart from
	// path lines.
	args := []string{
		"-C", repoRoot, "log",
		"--date=unix",
		"--name-only",
		"--pretty=format:\x1f%cd",
	}
	out, err := runGit(ctx, repoRoot, args)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	inWindow := false
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "\x1f") {
			when, perr := parseUnix(line[1:])
			if perr != nil {
				return nil, fmt.Errorf("git log: bad date %q: %w", line[1:], perr)
			}
			inWindow = !when.Before(cutoff)
			continue
		}
		path := strings.TrimSpace(line)
		if path == "" || !inWindow {
			continue
		}
		counts[path]++
	}
	return counts, nil
}

// FileHistory returns the commits that touched path within window,
// newest first. A zero window selects the 30-day default. path is
// interpreted relative to repoRoot.
func FileHistory(ctx context.Context, repoRoot, path string, window time.Duration) ([]Commit, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git log: repoRoot is empty")
	}
	if path == "" {
		return nil, fmt.Errorf("git log: path is empty")
	}
	// Record-separated, field-separated format keeps subjects with
	// embedded characters from being mis-parsed. The committer date
	// (%cd) drives windowing; the author date (%ad) is surfaced.
	const sep = "\x1f"
	cutoff := cutoffTime(window)
	args := []string{
		"-C", repoRoot, "log",
		"--date=unix",
		"--pretty=format:%H" + sep + "%an" + sep + "%ad" + sep + "%cd" + sep + "%s",
		"--", path,
	}
	out, err := runGit(ctx, repoRoot, args)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, sep, 5)
		if len(parts) != 5 {
			return nil, fmt.Errorf("git log: malformed record %q", line)
		}
		authored, err := parseUnix(parts[2])
		if err != nil {
			return nil, fmt.Errorf("git log: bad date %q: %w", parts[2], err)
		}
		committed, err := parseUnix(parts[3])
		if err != nil {
			return nil, fmt.Errorf("git log: bad date %q: %w", parts[3], err)
		}
		if committed.Before(cutoff) {
			continue
		}
		commits = append(commits, Commit{
			Hash:    parts[0],
			Author:  parts[1],
			When:    authored,
			Subject: parts[4],
		})
	}
	// `git log` already emits newest-first; sort explicitly so output is
	// deterministic even if commits share a timestamp.
	sort.SliceStable(commits, func(i, j int) bool {
		if !commits[i].When.Equal(commits[j].When) {
			return commits[i].When.After(commits[j].When)
		}
		return commits[i].Hash < commits[j].Hash
	})
	return commits, nil
}

// cutoffTime is the oldest commit time included by window; commits
// before it are out-of-window. A zero window selects the 30-day default.
func cutoffTime(window time.Duration) time.Time {
	if window <= 0 {
		window = defaultWindow
	}
	return time.Now().Add(-window)
}

// parseUnix parses a `--date=unix` timestamp (seconds since epoch).
func parseUnix(s string) (time.Time, error) {
	secs, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(secs, 0).UTC(), nil
}

// runGit executes a git command scoped to repoRoot and returns stdout.
func runGit(ctx context.Context, repoRoot string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git log in %s: %w: %s", repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
