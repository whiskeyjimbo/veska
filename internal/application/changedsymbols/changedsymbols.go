// SPDX-License-Identifier: AGPL-3.0-only

// Package changedsymbols computes the set of code symbols added, removed, or
// modified between two arbitrary git refs on demand, bypassing the sqlite graph
// database.
package changedsymbols

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ChangedFilesBetweenFunc defines the callback signature to list changed files
// relative to repoRoot.
type ChangedFilesBetweenFunc func(ctx context.Context, repoRoot, refA, refB string) ([]string, error)

// FileAtRefFunc reads the file contents at a specific git ref. Absence of the
// file should return ErrFileAbsentAtRef to allow the diff to proceed.
type FileAtRefFunc func(ctx context.Context, repoRoot, ref, path string) ([]byte, error)

// ErrFileAbsentAtRef is returned when a file does not exist at a ref, allowing
// diffs of added or deleted files to proceed without throwing an error.
var ErrFileAbsentAtRef = errors.New("changedsymbols: file absent at ref")

// Change classifies one symbol's fate between ref_a and ref_b.
type Change string

const (
	ChangeAdded    Change = "added"
	ChangeRemoved  Change = "removed"
	ChangeModified Change = "modified"
)

// SymbolChange describes a single classified symbol. LineStart/LineEnd point to
// ref_b for additions/modifications and ref_a for removals.
type SymbolChange struct {
	SymbolPath string          `json:"symbol_path"`
	Name       string          `json:"name"`
	Kind       domain.NodeKind `json:"kind"`
	FilePath   string          `json:"file_path"`
	LineStart  int             `json:"line_start"`
	LineEnd    int             `json:"line_end"`
	Change     Change          `json:"change"`
}

type Result struct {
	Added    []SymbolChange `json:"added"`
	Removed  []SymbolChange `json:"removed"`
	Modified []SymbolChange `json:"modified"`
	// DegradedReasons flags occurrences where a change occurred but yielded no
	// symbol-level diff (e.g. whitespace edits).
	DegradedReasons []string `json:"degraded_reasons"`
}

type Service struct {
	parser       ports.CodeParser
	changedFiles ChangedFilesBetweenFunc
	fileAtRef    FileAtRefFunc
}

func NewService(parser ports.CodeParser, changedFiles ChangedFilesBetweenFunc, fileAtRef FileAtRefFunc) (*Service, error) {
	if parser == nil {
		return nil, errors.New("changedsymbols: parser is nil")
	}
	if changedFiles == nil {
		return nil, errors.New("changedsymbols: changedFiles is nil")
	}
	if fileAtRef == nil {
		return nil, errors.New("changedsymbols: fileAtRef is nil")
	}
	return &Service{parser: parser, changedFiles: changedFiles, fileAtRef: fileAtRef}, nil
}

// Diff compares symbol states between two refs.
func (s *Service) Diff(ctx context.Context, repoID, repoRoot, refA, refB string) (Result, error) {
	files, err := s.changedFiles(ctx, repoRoot, refA, refB)
	if err != nil {
		// Wrap the error to allow callers to inspect specific git failures using errors.Is.
		return Result{}, fmt.Errorf("changedsymbols: list changed files: %w", err)
	}
	res := Result{
		Added:           []SymbolChange{},
		Removed:         []SymbolChange{},
		Modified:        []SymbolChange{},
		DegradedReasons: []string{},
	}
	nonSymbolOnlyFiles := 0
	baselineUnreachable := false
	for _, path := range files {
		nodesA, aUnreachable, err := s.parseAtRef(ctx, repoID, repoRoot, refA, path)
		if err != nil {
			return Result{}, err
		}
		if aUnreachable {
			// If the baseline ref cannot be read, flag the session as degraded rather
			// than implying only non-symbol code changed.
			baselineUnreachable = true
		}
		nodesB, _, err := s.parseAtRef(ctx, repoID, repoRoot, refB, path)
		if err != nil {
			return Result{}, err
		}
		before := len(res.Added) + len(res.Removed) + len(res.Modified)
		classifyFile(path, nodesA, nodesB, &res)
		after := len(res.Added) + len(res.Removed) + len(res.Modified)
		if before == after {
			nonSymbolOnlyFiles++
		}
	}
	// Degraded reasons are only populated when the overall symbol diff is empty, to
	// prevent false positives in mixed commits.
	if len(res.Added)+len(res.Removed)+len(res.Modified) == 0 {
		switch {
		case baselineUnreachable:
			// Baseline unreadability takes precedence over non-symbol changes to
			// highlight missing history.
			res.DegradedReasons = append(res.DegradedReasons, DegradedReasonBaselineRefNotIndexed)
		case nonSymbolOnlyFiles > 0:
			res.DegradedReasons = append(res.DegradedReasons, DegradedReasonNonSymbolChangesOnly)
		}
	}
	sortChanges(res.Added)
	sortChanges(res.Removed)
	sortChanges(res.Modified)
	return res, nil
}

// DegradedReasonNonSymbolChangesOnly flags edits that only changed comments, whitespace, or imports.
const DegradedReasonNonSymbolChangesOnly = "non_symbol_changes_only"

// DegradedReasonBaselineRefNotIndexed flags errors loading the baseline tree
// (e.g. corrupt object store or unindexed ref).
const DegradedReasonBaselineRefNotIndexed = "baseline_ref_not_indexed"

// DegradedReasonNoParentCommit flags when a single-commit repo diff fell back to
// an empty-tree SHA.
const DegradedReasonNoParentCommit = "no_parent_commit_used_empty_tree"

// parseAtRef reads and parses a path. It distinguishes file absence (expected)
// from tree unreadability (degraded).
func (s *Service) parseAtRef(ctx context.Context, repoID, repoRoot, ref, path string) (map[string]*domain.Node, bool, error) {
	content, err := s.fileAtRef(ctx, repoRoot, ref, path)
	if err != nil {
		if errors.Is(err, ErrFileAbsentAtRef) {
			return map[string]*domain.Node{}, false, nil
		}
		return map[string]*domain.Node{}, true, nil
	}
	pr, err := s.parser.ParseFile(ctx, repoID, path, content)
	if err != nil {
		return nil, false, fmt.Errorf("changedsymbols: parse %s at %s: %w", path, ref, err)
	}
	// Filter out KindChunk nodes since they represent gaps (comment/whitespace)
	// rather than user-facing symbols.
	out := make(map[string]*domain.Node, len(pr.Nodes))
	for _, n := range pr.Nodes {
		if n.Kind == domain.KindChunk {
			continue
		}
		out[symbolKey(n)] = n
	}
	return out, false, nil
}

func classifyFile(path string, a, b map[string]*domain.Node, res *Result) {
	for key, nb := range b {
		na, ok := a[key]
		if !ok {
			res.Added = append(res.Added, toChange(path, nb, ChangeAdded))
			continue
		}
		if contentHash(na) != contentHash(nb) {
			res.Modified = append(res.Modified, toChange(path, nb, ChangeModified))
		}
	}
	for key, na := range a {
		if _, ok := b[key]; !ok {
			res.Removed = append(res.Removed, toChange(path, na, ChangeRemoved))
		}
	}
}

// symbolKey generates a unique key combining path and symbol name to correlate
// symbols across refs.
func symbolKey(n *domain.Node) string {
	return n.Path + "::" + n.Name
}

// contentHash computes a stable digest of a node's body to detect modifications.
func contentHash(n *domain.Node) string {
	if n.ContentHash != nil {
		return string(*n.ContentHash)
	}
	if n.RawContent != nil {
		sum := sha256.Sum256([]byte(*n.RawContent))
		return hex.EncodeToString(sum[:])
	}
	return ""
}

func toChange(path string, n *domain.Node, c Change) SymbolChange {
	sc := SymbolChange{
		SymbolPath: symbolKey(n),
		Name:       n.Name,
		Kind:       n.Kind,
		FilePath:   path,
		Change:     c,
	}
	if n.Lines != nil {
		sc.LineStart = n.Lines.Start
		sc.LineEnd = n.Lines.End
	}
	return sc
}

func sortChanges(cs []SymbolChange) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].SymbolPath < cs[j].SymbolPath })
}
