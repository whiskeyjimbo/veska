// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// history.go provides commit-history queries to analyze change frequency and commit history.
// We query the Git log directly rather than relying on auxiliary databases, filtering and windowing
// in-process to guarantee deterministic sorting.

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

// defaultWindow defines the default look-back window of 30 days applied if a duration of zero is specified.
const defaultWindow = 30 * 24 * time.Hour

// Commit represents a single commit that modified a queried file path, exposing only read-only metadata.
type Commit struct {
	Hash    string    // full commit hash
	Author  string    // author name
	When    time.Time // author date
	Subject string    // commit subject line
}

// ChangeCounts aggregates the number of commits modifying each file within the look-back window.
// Filtering is performed in-process rather than utilizing `git log --since` because `--since` stops
// traversal at the first out-of-window commit, which would hide older in-window ancestors if HEAD
// is out-of-window. An empty repoRoot returns an error to prevent execution in incorrect contexts.
func ChangeCounts(ctx context.Context, repoRoot string, window time.Duration) (map[string]int, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git log: repoRoot is empty")
	}
	cutoff := cutoffTime(window)
	// We format output with a leading record-separator character to clearly distinguish
	// commit header dates from file paths during in-process parsing.
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

// FileHistory retrieves the commit history for a specific file path within the look-back window, sorted newest first.
func FileHistory(ctx context.Context, repoRoot, path string, window time.Duration) ([]Commit, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git log: repoRoot is empty")
	}
	if path == "" {
		return nil, fmt.Errorf("git log: path is empty")
	}
	// We utilize a custom field-separated format to prevent subjects containing newlines or special characters from corrupting parsing.
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
	// Sort commits explicitly by author date and then hash to ensure deterministic output even when commits share a timestamp.
	sort.SliceStable(commits, func(i, j int) bool {
		if !commits[i].When.Equal(commits[j].When) {
			return commits[i].When.After(commits[j].When)
		}
		return commits[i].Hash < commits[j].Hash
	})
	return commits, nil
}

// cutoffTime calculates the threshold timestamp beyond which commits are excluded.
func cutoffTime(window time.Duration) time.Time {
	if window <= 0 {
		window = defaultWindow
	}
	return time.Now().Add(-window)
}

// parseUnix parses a Unix timestamp formatted as seconds since epoch into a Time value.
func parseUnix(s string) (time.Time, error) {
	secs, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(secs, 0).UTC(), nil
}

// runGit executes a git command scoped to the repository root and returns its output.
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
