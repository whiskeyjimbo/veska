package fs

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DefaultIgnorePatterns are always excluded regardless of .engramignore.
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
}

// IgnoreList is the merged result of default patterns and a repo's .engramignore file.
type IgnoreList struct {
	patterns []string
}

// Load reads .engramignore from repoRoot (if it exists) and returns an IgnoreList
// merging DefaultIgnorePatterns with the file's patterns.
// Lines starting with # and blank lines are skipped.
// Returns a list of default patterns only if the file doesn't exist.
func Load(repoRoot string) (*IgnoreList, error) {
	patterns := make([]string, len(DefaultIgnorePatterns))
	copy(patterns, DefaultIgnorePatterns)

	path := filepath.Join(repoRoot, ".engramignore")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &IgnoreList{patterns: patterns}, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return &IgnoreList{patterns: patterns}, nil
}

// ShouldIgnore returns true if the given path (relative or absolute) matches
// any pattern in the list. Matching rules:
//   - Pattern ending in "/" matches any path component of that name (directory match)
//   - Pattern with "*" uses filepath.Match semantics (glob)
//   - Otherwise, exact substring match on the path
func (il *IgnoreList) ShouldIgnore(path string) bool {
	// Normalise to forward slashes for consistent matching.
	normalised := filepath.ToSlash(path)

	for _, pat := range il.patterns {
		if strings.HasSuffix(pat, "/") {
			// Directory pattern: match if any path component equals the dir name.
			dir := strings.TrimSuffix(pat, "/")
			// Check each component of the path.
			parts := strings.Split(normalised, "/")
			for _, part := range parts {
				if part == dir {
					return true
				}
			}
			continue
		}

		if strings.ContainsRune(pat, '*') {
			// Glob pattern: try matching against the base name and the full path.
			base := filepath.Base(normalised)
			if matched, _ := filepath.Match(pat, base); matched {
				return true
			}
			// Also try matching full path segments for patterns like "internal/*.go"
			if matched, _ := filepath.Match(pat, normalised); matched {
				return true
			}
			continue
		}

		// Substring match.
		if strings.Contains(normalised, pat) {
			return true
		}
	}
	return false
}

// Patterns returns a copy of all patterns (default + user).
func (il *IgnoreList) Patterns() []string {
	out := make([]string, len(il.patterns))
	copy(out, il.patterns)
	return out
}
