// Package changedsymbols computes the set of code symbols added, removed,
// or modified between two arbitrary git refs.
//
// It deliberately sidesteps the promoted SQLite graph: rather than reading
// a stored per-commit history substrate (which V2 does not have), the
// Service parses each changed file at both refs on demand via the
// tree-sitter CodeParser and diffs the resulting symbol sets by symbol
// path. This makes eng_find_changed_symbols a pure read-only,
// history-substrate-free tool.
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

// ChangedFilesBetweenFunc lists the files that differ between two git
// refs, with paths relative to repoRoot. It mirrors the injected
// func-type pattern used by blastradius.ChangedFilesFunc so the
// application layer does not import internal/infrastructure/git.
type ChangedFilesBetweenFunc func(ctx context.Context, repoRoot, refA, refB string) ([]string, error)

// FileAtRefFunc reads the content of path at the given git ref. When the
// file does not exist at that ref it must return (nil, err) where err
// satisfies errors.Is against the adapter's "not present" sentinel —
// the Service treats any error here as "file absent at that ref" and
// proceeds with an empty symbol set for that side.
type FileAtRefFunc func(ctx context.Context, repoRoot, ref, path string) ([]byte, error)

// Change classifies one symbol's fate between ref_a and ref_b.
type Change string

const (
	// ChangeAdded marks a symbol present at ref_b but not ref_a.
	ChangeAdded Change = "added"
	// ChangeRemoved marks a symbol present at ref_a but not ref_b.
	ChangeRemoved Change = "removed"
	// ChangeModified marks a symbol present at both refs whose content
	// hash differs.
	ChangeModified Change = "modified"
)

// SymbolChange describes a single classified symbol.
type SymbolChange struct {
	SymbolPath string          `json:"symbol_path"`
	Name       string          `json:"name"`
	Kind       domain.NodeKind `json:"kind"`
	FilePath   string          `json:"file_path"`
	Change     Change          `json:"change"`
}

// Result is the envelope returned by Service.Diff. Each slice is sorted
// by symbol path for deterministic output.
type Result struct {
	Added    []SymbolChange `json:"added"`
	Removed  []SymbolChange `json:"removed"`
	Modified []SymbolChange `json:"modified"`
}

// Service orchestrates the changed-files → parse-both-refs → diff flow.
// It is stateless and safe for concurrent callers.
type Service struct {
	parser       ports.CodeParser
	changedFiles ChangedFilesBetweenFunc
	fileAtRef    FileAtRefFunc
}

// NewService constructs a Service. All three dependencies are required.
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

// Diff reports the symbols added, removed, and modified between refA and
// refB in the single repo rooted at repoRoot. repoID is passed to the
// parser for node-ID derivation; branch is accepted for standard scoping
// but does not affect the on-demand ref parsing.
func (s *Service) Diff(ctx context.Context, repoID, repoRoot, refA, refB string) (Result, error) {
	files, err := s.changedFiles(ctx, repoRoot, refA, refB)
	if err != nil {
		// Pass the wrapped error through unchanged so callers can use
		// errors.Is to distinguish "unknown revision" (e.g. HEAD~1 on a
		// single-commit repo) from a generic git failure (solov2-dr31).
		return Result{}, fmt.Errorf("changedsymbols: list changed files: %w", err)
	}
	var res Result
	for _, path := range files {
		nodesA, err := s.parseAtRef(ctx, repoID, repoRoot, refA, path)
		if err != nil {
			return Result{}, err
		}
		nodesB, err := s.parseAtRef(ctx, repoID, repoRoot, refB, path)
		if err != nil {
			return Result{}, err
		}
		classifyFile(path, nodesA, nodesB, &res)
	}
	sortChanges(res.Added)
	sortChanges(res.Removed)
	sortChanges(res.Modified)
	return res, nil
}

// parseAtRef reads path at ref and parses it. A file absent at the ref
// (e.g. added or deleted between the two refs) yields an empty map and
// no error, so the caller still classifies the present side correctly.
func (s *Service) parseAtRef(ctx context.Context, repoID, repoRoot, ref, path string) (map[string]*domain.Node, error) {
	content, err := s.fileAtRef(ctx, repoRoot, ref, path)
	if err != nil {
		// Any read failure is treated as "file not present at this ref":
		// the adapter returns a sentinel-wrapped error for genuine
		// absence, and a missing side is the expected case for an
		// added/deleted file.
		return map[string]*domain.Node{}, nil
	}
	pr, err := s.parser.ParseFile(ctx, repoID, path, content)
	if err != nil {
		return nil, fmt.Errorf("changedsymbols: parse %s at %s: %w", path, ref, err)
	}
	out := make(map[string]*domain.Node, len(pr.Nodes))
	for _, n := range pr.Nodes {
		out[symbolKey(n)] = n
	}
	return out, nil
}

// classifyFile diffs the two symbol maps of one file into res.
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

// symbolKey is the cross-ref identity of a node: its symbol path (a
// node's Path is its file; the Name disambiguates symbols within a
// file). Both refs are diffed with the same key.
func symbolKey(n *domain.Node) string {
	return n.Path + "::" + n.Name
}

// contentHash returns a stable digest of the node's body used to detect
// "modified" symbols. The parser may populate either ContentHash or
// RawContent (the Go parser sets only RawContent); when neither is set
// the digest is "" and the symbol is reported as unchanged.
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
	return SymbolChange{
		SymbolPath: symbolKey(n),
		Name:       n.Name,
		Kind:       n.Kind,
		FilePath:   path,
		Change:     c,
	}
}

func sortChanges(cs []SymbolChange) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].SymbolPath < cs[j].SymbolPath })
}
