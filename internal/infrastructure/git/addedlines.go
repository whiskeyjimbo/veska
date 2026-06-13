// addedlines.go exposes AddedLinesForCommit — a thin os/exec wrapper that
// parses `git diff` unified/patch output to recover the newly-added ("+")
// lines of a commit, keyed by repo-root-relative file path.
//
// The secrets-scan check (M7) consumes this: it must scan only lines a
// commit introduced, never pre-existing context or removed lines.

package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Line is a single newly-added line of a diff: its line number in the
// new-file (post-commit) revision plus the line's text (without the
// leading "+" diff marker and without a trailing newline).
type Line struct {
	Number int
	Text   string
}

// AddedLinesForCommit returns the added lines introduced by commitSHA,
// keyed by repo-root-relative file path. It runs `git diff <sha>^ <sha>`
// and parses the unified patch output, collecting only "+"-prefixed body
// lines and assigning each its new-file line number from the surrounding
// "@@" hunk header.
//
// Files with no added lines (pure deletions) are omitted from the map.
// For a repository's first commit (no parent), the diff is taken against
// the empty tree so every line counts as added.
//
// An empty repoRoot or commitSHA returns an error rather than silently
// shelling out against the process cwd or HEAD.
func AddedLinesForCommit(ctx context.Context, repoRoot, commitSHA string) (map[string][]Line, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git diff: repoRoot is empty")
	}
	if commitSHA == "" {
		return nil, fmt.Errorf("git diff: commitSHA is empty")
	}
	// `git diff-tree` against the commit's first parent (or the empty tree
	// for a root commit). --root makes the root commit emit a full add diff;
	// -p produces a unified patch; -U0 keeps only changed lines (no context).
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

// AddedLinesBetween returns the lines added between refA and refB (the net
// "+"-side of `git diff -U0 refA refB`), keyed by repo-root-relative file path.
// Unlike AddedLinesForCommit (a single commit vs its parent) this spans an
// arbitrary ref RANGE, so the net-new security gate (solov2-zvh6.1) scans only
// what a candidate actually introduces over its base — a secret added then
// removed within the range does not appear, because the diff is net.
//
// Files with no added lines (pure deletions) are omitted. An empty repoRoot,
// refA, or refB returns an error rather than shelling out against defaults.
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

// parseAddedLines walks unified-patch text and collects "+"-prefixed body
// lines per file, numbering them from each "@@" hunk header's new-file
// start. It is split out from AddedLinesForCommit so it can be unit-tested
// without shelling out to git.
func parseAddedLines(patch string) map[string][]Line {
	out := make(map[string][]Line)
	var (
		curFile string
		newLine int
	)
	for raw := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ "):
			// "+++ b/path" — the new-file path. "/dev/null" means the file
			// was deleted; leave curFile empty so its hunks are skipped.
			curFile = newFilePath(raw)
		case strings.HasPrefix(raw, "@@ "):
			newLine = parseHunkNewStart(raw)
		case strings.HasPrefix(raw, "+++"):
			// Defensive: a body line "+++" is impossible under -U0 (only
			// changed lines appear) but never treat a header-like line as
			// content.
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

// newFilePath extracts the repo-root-relative path from a "+++ b/path"
// diff header, stripping git's conventional "b/" prefix. A "/dev/null"
// target (deleted file) yields the empty string.
func newFilePath(header string) string {
	p := strings.TrimSpace(strings.TrimPrefix(header, "+++ "))
	if p == "/dev/null" {
		return ""
	}
	p = strings.TrimPrefix(p, "b/")
	return p
}

// parseHunkNewStart extracts the new-file starting line number from a
// unified hunk header of the form "@@ -a,b +c,d @@ ...". It returns the
// value of c; on any parse failure it returns 1 (the conservative
// first-line fallback).
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
