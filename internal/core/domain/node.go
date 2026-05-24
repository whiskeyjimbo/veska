package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// KindVariable is a top-level (package-scope) var declaration. Captured
	// so framework patterns where the API surface lives in initialised vars
	// — cobra command trees (`var rootCmd = &cobra.Command{...}`), gin/echo
	// router globals, viper config singletons — appear in eng_find_symbol /
	// eng_get_file_nodes and become navigable from agent tools (solov2-b7wt).
	KindVariable NodeKind = "variable"
	// KindChunk is a non-declaration source region — package-level
	// vars, file-top comments, init() guts, anything between symbol
	// declarations. Chunks live alongside symbol nodes so the existing
	// embedder / FTS / search pipeline picks them up without special
	// casing, but they are excluded from entry_points and the
	// rerank definition-boost (solov2-jyt).
	KindChunk NodeKind = "chunk"
)

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
	Language    *string
	Exported    *bool
}

// NodeOption is a functional option applied during Node construction.
// Options are applied in order; the first error is returned immediately.
type NodeOption func(*Node) error

// WithSignature sets the optional method/function signature.
func WithSignature(sig string) NodeOption {
	return func(n *Node) error {
		n.Signature = &sig
		return nil
	}
}

// WithLines sets the optional 1-indexed line range.
// Returns an error if start or end is less than 1, or if start > end.
func WithLines(lr LineRange) NodeOption {
	return func(n *Node) error {
		if lr.Start < 1 {
			return fmt.Errorf("domain: LineRange.Start must be ≥ 1, got %d", lr.Start)
		}
		if lr.End < 1 {
			return fmt.Errorf("domain: LineRange.End must be ≥ 1, got %d", lr.End)
		}
		if lr.Start > lr.End {
			return fmt.Errorf("domain: LineRange.Start (%d) must be ≤ End (%d)", lr.Start, lr.End)
		}
		n.Lines = &lr
		return nil
	}
}

// WithRawContent sets the raw source text.  If ContentHash is already set it
// must equal sha256(content); otherwise an error is returned.
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

// WithContentHash sets a pre-computed SHA-256 content hash.  If RawContent is
// already set the hash must equal sha256(raw_content); otherwise an error is
// returned.
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

// WithLanguage sets the programming language for the node.
func WithLanguage(lang string) NodeOption {
	return func(n *Node) error {
		n.Language = &lang
		return nil
	}
}

// WithExported sets the exported/visibility flag.
func WithExported(exported bool) NodeOption {
	return func(n *Node) error {
		n.Exported = &exported
		return nil
	}
}

// validateHashMatchesContent returns an error when h != sha256(content).
func validateHashMatchesContent(h ContentHash, content string) error {
	sum := sha256.Sum256([]byte(content))
	expected := hex.EncodeToString(sum[:])
	if string(h) != expected {
		return fmt.Errorf("domain: content_hash %q does not match sha256(raw_content) %q", h, expected)
	}
	return nil
}

// NewNode constructs a Node, validates invariants, and applies functional options.
// Required fields are id, path, name, and kind.  An error is returned for any
// invariant violation.
func NewNode(id, path, name string, kind NodeKind, opts ...NodeOption) (*Node, error) {
	if id == "" {
		return nil, errors.New("domain: Node id must not be empty")
	}

	n := &Node{
		ID:   NodeID(id),
		Path: path,
		Name: name,
		Kind: kind,
	}

	for _, opt := range opts {
		if err := opt(n); err != nil {
			return nil, err
		}
	}

	return n, nil
}
