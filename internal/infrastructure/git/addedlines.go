// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package git provides wrappers and parsers for git command line operations,
// including diff analysis, repository watching, and reference reconciliation.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Line represents a single newly-added line of a diff, including its line number in the
// post-commit revision and its text content without the leading "+" marker.
type Line struct {
	Number int
	Text   string
}

// AddedLinesForCommit returns the added lines introduced by the specified commit,
// keyed by repository-root-relative file paths. It runs `git diff-tree` against the parent
// and parses the unified patch output, collecting "+"-prefixed content lines and mapping them
// to their line numbers. For a repository's first commit, the diff is taken against the empty
// tree so all lines are considered added. An empty repoRoot or commitSHA returns an error to
// prevent executing git commands in arbitrary directories.
func AddedLinesForCommit(ctx context.Context, repoRoot, commitSHA string) (map[string][]Line, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git diff: repoRoot is empty")
	}
	if commitSHA == "" {
		return nil, fmt.Errorf("git diff: commitSHA is empty")
	}
	// Query git diff-tree against the parent commit. We use --root so that the first commit
	// is processed correctly, and -U0 to suppress unchanged context lines.
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"diff-tree", "--root", "-p", "-U0", "--no-color", commitSHA)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff-tree %s in %s: %w: %s",
			commitSHA, repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	return parseAddedLines(stdout.String()), nil
}

// AddedLinesBetween returns the net lines added between two references, keyed by
// repository-root-relative file paths. This method computes the net changes over an
// arbitrary range, meaning transient additions that are deleted within the range are
// ignored. An empty repoRoot, refA, or refB returns an error to prevent execution in
// incorrect working directories.
func AddedLinesBetween(ctx context.Context, repoRoot, refA, refB string) (map[string][]Line, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git diff: repoRoot is empty")
	}
	if refA == "" || refB == "" {
		return nil, fmt.Errorf("git diff: refA and refB are required")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"diff", "-U0", "--no-color", refA, refB)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff %s %s in %s: %w: %s",
			refA, refB, repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	return parseAddedLines(stdout.String()), nil
}

// parseAddedLines parses unified patch text and aggregates added lines per file, assigning
// line numbers relative to the corresponding hunk header. This function is decoupled from
// subprocess execution to simplify unit testing.
func parseAddedLines(patch string) map[string][]Line {
	out := make(map[string][]Line)
	var (
		curFile string
		newLine int
	)
	for raw := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ "):
			// Extract the new-file path. If the file was deleted (indicated by "/dev/null"),
			// we leave curFile empty to skip processing any subsequent hunks.
			curFile = newFilePath(raw)
		case strings.HasPrefix(raw, "@@ "):
			newLine = parseHunkNewStart(raw)
		case strings.HasPrefix(raw, "+++"):
			// Skip metadata headers to prevent them from being incorrectly classified as content.
		case strings.HasPrefix(raw, "+"):
			if curFile == "" {
				continue
			}
			out[curFile] = append(out[curFile], Line{
				Number: newLine,
				Text:   raw[1:],
			})
			newLine++
		}
	}
	return out
}

// newFilePath extracts the relative file path from a unified diff header line, stripping the
// conventional prefix. It returns an empty string if the target is /dev/null.
func newFilePath(header string) string {
	p := strings.TrimSpace(strings.TrimPrefix(header, "+++ "))
	if p == "/dev/null" {
		return ""
	}
	p = strings.TrimPrefix(p, "b/")
	return p
}

// parseHunkNewStart extracts the starting line number for the post-change file from a unified
// diff hunk header. It returns a conservative fallback of 1 if parsing fails.
func parseHunkNewStart(header string) int {
	// header: "@@ -12,0 +13,2 @@ optional context"
	fields := strings.FieldsSeq(header)
	for f := range fields {
		if !strings.HasPrefix(f, "+") {
			continue
		}
		spec := strings.TrimPrefix(f, "+")
		if i := strings.IndexByte(spec, ','); i >= 0 {
			spec = spec[:i]
		}
		if n, err := strconv.Atoi(spec); err == nil {
			return n
		}
	}
	return 1
}
