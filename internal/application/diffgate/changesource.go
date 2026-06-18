// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"context"
	"errors"
	"fmt"
)

// FileChange is one file's state in a candidate change. Content carries the
// file's full bytes as they exist in the candidate; for a deleted file
// Content is nil and Deleted is true. The Indexer parses Content into the
// candidate overlay (a deleted file stages an empty entry, recording "this
// file now has no symbols").
type FileChange struct {
	Path    string // repo-relative path
	Content []byte // full candidate content; nil when Deleted
	Deleted bool
}

// ChangeSource yields a candidate change as a set of per-file new contents,
// abstracted from HOW the change is obtained. v1 is a git ref/worktree
// (RefChangeSource); the deferred raw-unified-diff source
// is a second implementation. The Indexer and the downstream verify/guard
// consumers see only this interface, never the underlying mechanism - so a
// new source slots in with no change to their code (AC3).
type ChangeSource interface {
	// Changes returns the files the candidate adds, modifies, or deletes.
	// Order is the source's own; the Indexer does not depend on it.
	Changes(ctx context.Context) ([]FileChange, error)
}

// ChangedFilesBetweenFunc lists the files that differ between two git refs,
// with paths relative to repoRoot. It mirrors git.ChangedFilesBetween and the
// injected func-type used by changedsymbols/blastradius so the application
// layer does not import internal/infrastructure/git.
type ChangedFilesBetweenFunc func(ctx context.Context, repoRoot, refA, refB string) ([]string, error)

// FileAtRefFunc reads path's content at the given git ref. On legitimate
// absence (a file added or deleted between the two refs) adapters must return
// (nil, err) where err satisfies errors.Is against ErrFileAbsentAtRef; the
// RefChangeSource treats that as a deletion on the candidate side. Any other
// error is propagated.
type FileAtRefFunc func(ctx context.Context, repoRoot, ref, path string) ([]byte, error)

// ErrFileAbsentAtRef is the sentinel adapters wrap when reporting that a path
// does not exist at the requested ref. Distinguishing it from a generic read
// failure lets RefChangeSource tell "file deleted in the candidate" apart from
// "ref unreadable". It mirrors changedsymbols.ErrFileAbsentAtRef; diffgate
// keeps its own so the package is self-contained.
var ErrFileAbsentAtRef = errors.New("diffgate: file absent at ref")

// RefChangeSource is the v1 ChangeSource: the change is the diff between a
// base ref and a candidate ref/worktree in the single repo at repoRoot. It is
// network-free - both the changed-file listing and the per-file reads go
// through injected git adapters, no embedder is touched.
type RefChangeSource struct {
	repoRoot     string
	baseRef      string
	candidateRef string
	changedFiles ChangedFilesBetweenFunc
	fileAtRef    FileAtRefFunc
}

// NewRefChangeSource constructs a RefChangeSource. All dependencies are
// required; repoRoot/baseRef/candidateRef identify the change and the two
// funcs are the git adapters that read it.
func NewRefChangeSource(repoRoot, baseRef, candidateRef string, changedFiles ChangedFilesBetweenFunc, fileAtRef FileAtRefFunc) (*RefChangeSource, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("%w: repoRoot is empty", ErrMissingDependency)
	}
	if baseRef == "" || candidateRef == "" {
		return nil, fmt.Errorf("%w: baseRef/candidateRef is empty", ErrMissingDependency)
	}
	if changedFiles == nil {
		return nil, fmt.Errorf("%w: changedFiles func is nil", ErrMissingDependency)
	}
	if fileAtRef == nil {
		return nil, fmt.Errorf("%w: fileAtRef func is nil", ErrMissingDependency)
	}
	return &RefChangeSource{
		repoRoot:     repoRoot,
		baseRef:      baseRef,
		candidateRef: candidateRef,
		changedFiles: changedFiles,
		fileAtRef:    fileAtRef,
	}, nil
}

// Changes lists the files differing between base and candidate, then reads
// each at the candidate ref. A file absent at the candidate (sentinel-wrapped
// ErrFileAbsentAtRef) is reported as a deletion; any other read error aborts.
func (s *RefChangeSource) Changes(ctx context.Context) ([]FileChange, error) {
	paths, err := s.changedFiles(ctx, s.repoRoot, s.baseRef, s.candidateRef)
	if err != nil {
		return nil, fmt.Errorf("diffgate: list changed files: %w", err)
	}
	out := make([]FileChange, 0, len(paths))
	for _, p := range paths {
		content, err := s.fileAtRef(ctx, s.repoRoot, s.candidateRef, p)
		if err != nil {
			if errors.Is(err, ErrFileAbsentAtRef) {
				out = append(out, FileChange{Path: p, Deleted: true})
				continue
			}
			return nil, fmt.Errorf("diffgate: read %s at %s: %w", p, s.candidateRef, err)
		}
		out = append(out, FileChange{Path: p, Content: content})
	}
	return out, nil
}
