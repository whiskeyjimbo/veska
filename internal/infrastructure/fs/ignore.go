// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package fs contains filesystem infrastructure adapters, including
// gitignore-style ignore-file matching.
package fs

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// DefaultIgnorePatterns lists paths that are always excluded from indexing.
// This includes common AI-agent worktree directories (such as .claude, .cursor, and .aider)
// to prevent cold scans from indexing duplicate symbols across task worktrees.
// Duplicate symbols inflate the embedder queue and degrade search ranking quality.
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

// IgnoreList merges default ignore patterns with those defined in a repository's .veskaignore file.
// It applies standard .gitignore matching rules, including globbing and last-match-wins negation.
// Default patterns are evaluated first, allowing user-defined rules to override them.
type IgnoreList struct {
	patterns []string
	matcher  *ignore.GitIgnore
}

// NewIgnoreListFromPatterns constructs an IgnoreList directly from a slice of patterns.
func NewIgnoreListFromPatterns(patterns []string) *IgnoreList {
	p := make([]string, len(patterns))
	copy(p, patterns)
	return newIgnoreList(p)
}

// newIgnoreList compiles patterns into a gitignore matcher. The caller must not retain
// or modify the patterns slice after calling this function.
func newIgnoreList(patterns []string) *IgnoreList {
	return &IgnoreList{
		patterns: patterns,
		matcher:  ignore.CompileIgnoreLines(patterns...),
	}
}

// Load reads patterns from .veskaignore in the repository root and merges them with DefaultIgnorePatterns.
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
		// Comments start with "#". Negation patterns beginning with "!" are preserved.
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

// ShouldIgnore reports whether the relative path matches any ignore patterns.
// Directory paths should be passed with a trailing slash to match directory-only patterns.
// Note that negation patterns starting with "!" cannot re-include files within directories
// that have already been excluded, mirroring git's own tree walking behavior.
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
