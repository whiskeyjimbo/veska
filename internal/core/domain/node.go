// SPDX-License-Identifier: AGPL-3.0-only

package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// NodeID is a unique identifier for a Node.
type NodeID string

// NodeKind is a closed enum of the kinds of code symbols a Node can represent.
type NodeKind string

const (
	KindFunction  NodeKind = "function"
	KindMethod    NodeKind = "method"
	KindType      NodeKind = "type"
	KindStruct    NodeKind = "struct"
	KindInterface NodeKind = "interface"
	KindClass     NodeKind = "class"
	KindModule    NodeKind = "module"
	KindPackage   NodeKind = "package"
	KindFile      NodeKind = "file"
	KindField     NodeKind = "field"
	KindTest      NodeKind = "test"
	// KindVariable represents a top-level variable declaration. This captures
	// framework patterns where the API surface is exposed via initialized variables
	// like command trees or config singletons.
	KindVariable NodeKind = "variable"
	// KindCommand represents a CLI command word (e.g. from cobra or urfave command
	// specs) rather than the Go variable name, allowing agents to navigate the
	// command tree structural hierarchy.
	KindCommand NodeKind = "command"
	// KindRoute represents an HTTP route path surfaced from router registration calls.
	KindRoute NodeKind = "route"
	// KindChunk represents a non-declaration source region. Chunks are indexed for
	// search alongside symbols but are excluded from entry point analysis and search
	// reranking boosts.
	KindChunk NodeKind = "chunk"
)

// validNodeKinds defines the closed set of valid NodeKind values enforced by NewNode.
var validNodeKinds = map[NodeKind]struct{}{
	KindFunction:  {},
	KindMethod:    {},
	KindType:      {},
	KindStruct:    {},
	KindInterface: {},
	KindClass:     {},
	KindModule:    {},
	KindPackage:   {},
	KindFile:      {},
	KindField:     {},
	KindTest:      {},
	KindVariable:  {},
	KindCommand:   {},
	KindRoute:     {},
	KindChunk:     {},
}

// ContentHash is a hex-encoded SHA-256 digest of a node's raw content.
type ContentHash string

// LineRange describes the 1-indexed start/end lines of a node in its source file.
type LineRange struct {
	Start int
	End   int
}

// Node is a code-graph entity representing a named symbol in a source file.
type Node struct {
	ID          NodeID
	Path        string
	Name        string
	Kind        NodeKind
	Signature   *string
	Lines       *LineRange
	RawContent  *string
	ContentHash *ContentHash
	// StructuralHash is a SHA-256 hash computed over normalized tokens, allowing
	// variable-renamed (Type-2) code duplicates to match even when raw ContentHash
	// differs.
	StructuralHash *ContentHash
	Language       *string
	Exported       *bool
	// External marks a node as sourced from a vendored or module-cache dependency
	// rather than first-party code.
	External *bool
	// ShortSummary is an optional natural-language one-liner describing the node,
	// produced either heuristically or by the LLM summary lane. Nil until the
	// summary lane runs; readers fall back to a heuristic. Bounded to
	// MaxShortSummaryRunes when present.
	ShortSummary *string
}

// MaxShortSummaryRunes bounds ShortSummary to the SOLO-09 §4.1 summary budget.
const MaxShortSummaryRunes = 280

type NodeOption func(*Node) error

// WithSignature sets the optional method or function signature.
func WithSignature(sig string) NodeOption {
	return func(n *Node) error {
		n.Signature = &sig
		return nil
	}
}

// WithLines sets the optional 1-indexed line range, validating that start is
// positive and less than or equal to end.
func WithLines(lr LineRange) NodeOption {
	return func(n *Node) error {
		if lr.Start < 1 {
			return fmt.Errorf("node: LineRange.Start must be ≥ 1, got %d", lr.Start)
		}
		if lr.End < 1 {
			return fmt.Errorf("node: LineRange.End must be ≥ 1, got %d", lr.End)
		}
		if lr.Start > lr.End {
			return fmt.Errorf("node: LineRange.Start (%d) must be ≤ End (%d)", lr.Start, lr.End)
		}
		n.Lines = &lr
		return nil
	}
}

// WithRawContent sets the raw source text, validating that it matches any
// pre-existing ContentHash.
func WithRawContent(raw string) NodeOption {
	return func(n *Node) error {
		n.RawContent = &raw
		if n.ContentHash != nil {
			if err := validateHashMatchesContent(*n.ContentHash, raw); err != nil {
				return err
			}
		}
		return nil
	}
}

// WithContentHash sets the pre-computed ContentHash, validating that it matches
// any pre-existing RawContent.
func WithContentHash(h ContentHash) NodeOption {
	return func(n *Node) error {
		if n.RawContent != nil {
			if err := validateHashMatchesContent(h, *n.RawContent); err != nil {
				return err
			}
		}
		n.ContentHash = &h
		return nil
	}
}

// WithStructuralHash sets the normalized structural hash computed from the AST.
func WithStructuralHash(h ContentHash) NodeOption {
	return func(n *Node) error {
		n.StructuralHash = &h
		return nil
	}
}

// WithLanguage sets the programming language for the node.
func WithLanguage(lang string) NodeOption {
	return func(n *Node) error {
		n.Language = &lang
		return nil
	}
}

// WithExported sets the exported visibility flag.
func WithExported(exported bool) NodeOption {
	return func(n *Node) error {
		n.Exported = &exported
		return nil
	}
}

// WithExternal marks the node as sourced from an external dependency.
func WithExternal(external bool) NodeOption {
	return func(n *Node) error {
		n.External = &external
		return nil
	}
}

// HeuristicSummary returns the deterministic fallback one-liner for a node:
// its signature when present, otherwise "<kind> <name>". The result is bounded
// to MaxShortSummaryRunes so it satisfies the same contract as a stored
// ShortSummary. Used by the default node projection when ShortSummary is nil
// and by the summary lane as the baseline the LLM upgrade is measured against.
func (n *Node) HeuristicSummary() string {
	s := ""
	if n.Signature != nil {
		s = strings.TrimSpace(*n.Signature)
	}
	if s == "" {
		s = strings.TrimSpace(string(n.Kind) + " " + n.Name)
	}
	return TruncateRunes(s, MaxShortSummaryRunes)
}

// TruncateRunes clips s to at most n runes, cutting on a rune boundary.
func TruncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// WithShortSummary sets the optional natural-language summary, rejecting a
// value longer than MaxShortSummaryRunes runes.
func WithShortSummary(summary string) NodeOption {
	return func(n *Node) error {
		if c := utf8.RuneCountInString(summary); c > MaxShortSummaryRunes {
			return fmt.Errorf("node: ShortSummary is %d runes, max is %d", c, MaxShortSummaryRunes)
		}
		n.ShortSummary = &summary
		return nil
	}
}

// validateHashMatchesContent verifies that the ContentHash matches the SHA-256 hash of the raw content.
func validateHashMatchesContent(h ContentHash, content string) error {
	sum := sha256.Sum256([]byte(content))
	expected := hex.EncodeToString(sum[:])
	if string(h) != expected {
		return fmt.Errorf("node: content_hash %q does not match sha256(raw_content) %q", h, expected)
	}
	return nil
}

// NodeSpec groups the required fields of a Node into a struct to prevent
// transposing adjacent same-typed parameters at construction call sites.
type NodeSpec struct {
	ID   string
	Path string
	Name string
	Kind NodeKind
}

// NewNode constructs a validated Node from the specification, verifying that
// spec.ID, spec.Path, and spec.Name are non-empty.
func NewNode(spec NodeSpec, opts ...NodeOption) (*Node, error) {
	if spec.ID == "" {
		return nil, errors.New("node: id must not be empty")
	}
	if spec.Path == "" {
		return nil, errors.New("node: path must not be empty")
	}
	if spec.Name == "" {
		return nil, errors.New("node: name must not be empty")
	}
	if _, ok := validNodeKinds[spec.Kind]; !ok {
		return nil, errors.New("node: invalid kind")
	}

	n := &Node{
		ID:   NodeID(spec.ID),
		Path: spec.Path,
		Name: spec.Name,
		Kind: spec.Kind,
	}

	for _, opt := range opts {
		if err := opt(n); err != nil {
			return nil, err
		}
	}

	// Derive the content hash from the raw content if it was not explicitly supplied,
	// ensuring that nodes with identical raw contents can be grouped correctly for
	// clone detection.
	if n.RawContent != nil && n.ContentHash == nil {
		sum := sha256.Sum256([]byte(*n.RawContent))
		h := ContentHash(hex.EncodeToString(sum[:]))
		n.ContentHash = &h
	}

	return n, nil
}
