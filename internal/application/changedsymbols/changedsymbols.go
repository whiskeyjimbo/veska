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

// FileAtRefFunc reads the content of path at the given git ref. On
// legitimate absence (file added/deleted between the two refs) adapters
// must return (nil, err) where err satisfies errors.Is against
// ErrFileAbsentAtRef — the Service treats that as "empty symbol set for
// that side" and proceeds. Any OTHER error is interpreted as "this ref's
// tree was unreadable" and propagated as the baseline_ref_not_indexed
// degraded reason (solov2-izh6.17).
type FileAtRefFunc func(ctx context.Context, repoRoot, ref, path string) ([]byte, error)

// ErrFileAbsentAtRef is the sentinel adapters must wrap when reporting
// that a file does not exist at the requested ref. Distinguishing it
// from a generic read failure lets the Service tell "file genuinely
// absent at ref" (an added/deleted file, expected) apart from "ref's
// tree is unreadable" (e.g. an unfetched commit, corrupted object
// store, or — in the user-facing rephrasing the bug report uses — a
// baseline ref whose tree was never indexed) (solov2-izh6.17).
var ErrFileAbsentAtRef = errors.New("changedsymbols: file absent at ref")

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

// SymbolChange describes a single classified symbol. LineStart/LineEnd are
// the symbol's line range at the *ref_b* side for added/modified, and at
// ref_a for removed — i.e. the location an editor should jump to so the
// user can read the symbol that exists at the surviving ref.
type SymbolChange struct {
	SymbolPath string          `json:"symbol_path"`
	Name       string          `json:"name"`
	Kind       domain.NodeKind `json:"kind"`
	FilePath   string          `json:"file_path"`
	LineStart  int             `json:"line_start"`
	LineEnd    int             `json:"line_end"`
	Change     Change          `json:"change"`
}

// Result is the envelope returned by Service.Diff. Each slice is sorted
// by symbol path for deterministic output.
type Result struct {
	Added    []SymbolChange `json:"added"`
	Removed  []SymbolChange `json:"removed"`
	Modified []SymbolChange `json:"modified"`
	// DegradedReasons surfaces advisory hints — currently only
	// "non_symbol_changes_only" when files were modified but produced no
	// symbol-level diff (typical for comment/whitespace/import-only
	// edits). Agents reading {added:[],removed:[],modified:[]} would
	// otherwise conclude "nothing changed" — solov2-u9os.
	DegradedReasons []string `json:"degraded_reasons"`
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
		// single-commit repo) from a generic git failure .
		return Result{}, fmt.Errorf("changedsymbols: list changed files: %w", err)
	}
	// Initialise with empty (non-nil) slices so JSON marshaling renders
	// each field as [] when no symbols changed in that bucket. The MCP
	// surface contract guarantees "empty result collections serialize as
	// [], never omitted" .
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
			// solov2-izh6.17: refA's tree couldn't be read for this file
			// via a non-absence error. Treat the baseline side as empty
			// for diff purposes but remember that we did NOT actually
			// observe its contents — so a downstream "no symbol diff"
			// outcome is reported as baseline_ref_not_indexed rather than
			// the misleading non_symbol_changes_only.
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
			// File was listed as changed but produced no symbol-level
			// diff — comments, whitespace, imports, or other gaps the
			// chunk-aware embedder cares about but the symbol-grain
			// caller does not.
			nonSymbolOnlyFiles++
		}
	}
	// solov2-qu6l: only surface the "non_symbol_changes_only" hint when
	// EVERY symbol bucket is empty. Previously the hint fired any time at
	// least one changed file had no symbol diff — which contradicted itself
	// in mixed commits (e.g. one .go file added a method AND a generated
	// .md file changed: the response had a real symbol diff yet still
	// claimed "non_symbol_changes_only"). The user-visible purpose of the
	// hint is "the diff is empty, but stuff did change — here's why"; once
	// the diff is non-empty the hint is just noise.
	if len(res.Added)+len(res.Removed)+len(res.Modified) == 0 {
		switch {
		case baselineUnreachable:
			// solov2-izh6.17: when refA's tree was unreadable for at
			// least one changed file, the empty symbol diff is most
			// honestly explained as "we never saw the baseline".
			// Emit this in PLACE of non_symbol_changes_only — the two
			// reasons are mutually exclusive at this layer (the latter
			// is a true-negative; this one is a can't-observe).
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

// DegradedReasonNonSymbolChangesOnly is emitted on Diff results when at
// least one changed file produced no symbol-level diff — typical for
// comment, whitespace, or import-only edits. Agents reading empty
// added/removed/modified buckets would otherwise interpret the result as
// "nothing changed" .
const DegradedReasonNonSymbolChangesOnly = "non_symbol_changes_only"

// DegradedReasonBaselineRefNotIndexed is emitted when ref_a's tree
// could not be read for at least one changed file via a non-absence
// error from the FileAtRefFunc adapter. Common causes: an unfetched
// commit, a corrupted object store, or — in the user-facing phrasing
// from the original bug report — a baseline ref whose tree was never
// indexed/promoted on this machine. Distinguishing it from
// non_symbol_changes_only stops the tool from telling agents "only
// comments/whitespace changed" when the real story is "we couldn't see
// the baseline at all" (solov2-izh6.17).
const DegradedReasonBaselineRefNotIndexed = "baseline_ref_not_indexed"

// DegradedReasonNoParentCommit is emitted when the default HEAD~1 base
// could not be resolved and the handler fell back to git's empty-tree SHA
// to diff against. Surfaces the fact that "every symbol shows as added"
// is a consequence of a single-commit repo, not a real wholesale change
// .
const DegradedReasonNoParentCommit = "no_parent_commit_used_empty_tree"

// parseAtRef reads path at ref and parses it. The second return value
// reports whether the read failed with a non-absence error (i.e. the
// ref's tree was unreadable for this file). A legitimately absent file
// (sentinel-wrapped errors.Is(err, ErrFileAbsentAtRef)) yields an empty
// map with unreachable=false; an unreachable read also yields an empty
// map but with unreachable=true so the caller can degrade the result
// with baseline_ref_not_indexed rather than non_symbol_changes_only
// (solov2-izh6.17).
func (s *Service) parseAtRef(ctx context.Context, repoID, repoRoot, ref, path string) (map[string]*domain.Node, bool, error) {
	content, err := s.fileAtRef(ctx, repoRoot, ref, path)
	if err != nil {
		if errors.Is(err, ErrFileAbsentAtRef) {
			// Sentinel-wrapped absence: file genuinely not present at
			// this ref (added/deleted between refs). Empty side, no
			// degraded reason.
			return map[string]*domain.Node{}, false, nil
		}
		// Non-absence read failure: we couldn't see the ref's tree for
		// this file. Empty side, flag the caller so it can emit
		// baseline_ref_not_indexed if the overall diff comes back empty.
		return map[string]*domain.Node{}, true, nil
	}
	pr, err := s.parser.ParseFile(ctx, repoID, path, content)
	if err != nil {
		return nil, false, fmt.Errorf("changedsymbols: parse %s at %s: %w", path, ref, err)
	}
	// solov2-u9os: chunks are an embedding-internal concept (KindChunk
	// covers comment/whitespace/import gaps between symbols so semantic
	// search can find non-declaration code). Including them here makes
	// "find_changed_symbols" emit chunk:N-M entries that aren't symbols
	// from the user's perspective. Filter them out; the degraded reason
	// "non_symbol_changes_only" still tells the caller the file
	// changed if NO real symbol differed.
	out := make(map[string]*domain.Node, len(pr.Nodes))
	for _, n := range pr.Nodes {
		if n.Kind == domain.KindChunk {
			continue
		}
		out[symbolKey(n)] = n
	}
	return out, false, nil
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
