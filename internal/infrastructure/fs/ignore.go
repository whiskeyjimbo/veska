package fs

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// DefaultIgnorePatterns are always excluded regardless of .veskaignore.
//
// solov2-v2zx: include common AI-agent worktree roots (`.claude/worktrees/`,
// `.git/worktrees/`, `.cursor/`, `.aider*/`). Without these, a cold scan
// inside a repo whose tools create per-task worktrees ends up indexing N
// copies of every symbol — once for the main tree, once for each worktree —
// which trashes search ranking and inflates the embedder queue. The agent
// worktree paths are conventional, not hard schema, so they belong with the
// other "almost always wrong to index" defaults like vendor/ and node_modules/.
var DefaultIgnorePatterns = []string{
	"vendor/",
	"node_modules/",
	".git/",
	".hg/",
	".svn/",
	"dist/",
	"build/",
	"out/",
	"__pycache__/",
	"*.pb.go",
	"*.gen.go",
	"testdata/",
	".claude/",
	".cursor/",
	".aider*/",
}

// IgnoreList is the merged result of default patterns and a repo's .veskaignore
// file, matched with standard .gitignore semantics (anchoring, recursive `**`
// globs, and last-match-wins negation via `!`). Patterns are kept in
// merge order (defaults first, user patterns last) so user lines can override
// defaults — significant because negation makes order meaningful.
type IgnoreList struct {
	patterns []string
	matcher  *ignore.GitIgnore
}

// NewIgnoreListFromPatterns creates an IgnoreList from the provided patterns
// without reading any file. Useful in tests and programmatic construction.
func NewIgnoreListFromPatterns(patterns []string) *IgnoreList {
	p := make([]string, len(patterns))
	copy(p, patterns)
	return newIgnoreList(p)
}

// newIgnoreList compiles patterns into a gitignore matcher. It takes ownership
// of the slice (callers must not retain it).
func newIgnoreList(patterns []string) *IgnoreList {
	return &IgnoreList{
		patterns: patterns,
		matcher:  ignore.CompileIgnoreLines(patterns...),
	}
}

// Load reads .veskaignore from repoRoot (if it exists) and returns an IgnoreList
// merging DefaultIgnorePatterns with the file's patterns.
// Lines starting with # and blank lines are skipped.
// Returns a list of default patterns only if the file doesn't exist.
func Load(repoRoot string) (*IgnoreList, error) {
	patterns := make([]string, len(DefaultIgnorePatterns))
	copy(patterns, DefaultIgnorePatterns)

	path := filepath.Join(repoRoot, ".veskaignore")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newIgnoreList(patterns), nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip blank lines and comments. Negation lines (`!`) are real
		// patterns under gitignore semantics, so only `#` is a comment.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return newIgnoreList(patterns), nil
}

// ShouldIgnore reports whether the given path matches the ignore list under
// standard .gitignore semantics (anchoring, `**` recursion, character classes,
// and last-match-wins negation). The path should be relative to the repo root;
// callers pass directory paths with a trailing slash so directory-only patterns
// (e.g. "vendor/") match. Forward and backslash separators are both accepted.
//
// Negation (`!`) re-includes a path at the pattern level, but matches git's own
// limitation during a tree walk: a cold scan that prunes an excluded directory
// (SkipDir) never visits its children, so `!secret/keep.txt` cannot re-surface
// a file under an already-excluded `secret/`.
func (il *IgnoreList) ShouldIgnore(path string) bool {
	if il.matcher == nil {
		return false
	}
	return il.matcher.MatchesPath(filepath.ToSlash(path))
}

// Patterns returns a copy of all patterns (default + user).
func (il *IgnoreList) Patterns() []string {
	out := make([]string, len(il.patterns))
	copy(out, il.patterns)
	return out
}
