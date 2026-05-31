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

// validNodeKinds is the closed set of recognised NodeKind values. NewNode
// rejects any kind outside this set, mirroring validSourceLayers / severityOrder
// in finding.go and validActorKinds in actor.go.
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
	Language    *string
	Exported    *bool
	// External marks a node sourced from a registered repo's vendored
	// or module-cache dependency rather than its first-party code
	// (solov2-bchl). Defaults to nil (i.e. first-party / unknown).
	// Stored as INTEGER 0/1 in the nodes table; read paths set it on
	// rehydrate so MCP responses can label hits without an extra
	// lookup.
	External *bool
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

// WithExternal marks the node as sourced from a vendored or
// module-cache dependency (solov2-bchl). nil keeps the default
// (first-party / unknown).
func WithExternal(external bool) NodeOption {
	return func(n *Node) error {
		n.External = &external
		return nil
	}
}

// validateHashMatchesContent returns an error when h != sha256(content).
func validateHashMatchesContent(h ContentHash, content string) error {
	sum := sha256.Sum256([]byte(content))
	expected := hex.EncodeToString(sum[:])
	if string(h) != expected {
		return fmt.Errorf("node: content_hash %q does not match sha256(raw_content) %q", h, expected)
	}
	return nil
}

// NodeSpec carries the required fields of a Node. It groups the constructor's
// positional arguments into a named struct so adjacent same-typed fields
// (ID/Path/Name) cannot be transposed at a call site, mirroring FindingSpec.
// Optional fields (signature, lines, content, language, exported, external)
// are still supplied via NodeOption.
type NodeSpec struct {
	ID   string
	Path string
	Name string
	Kind NodeKind
}

// NewNode constructs a Node from spec, validates invariants, and applies
// functional options. spec.ID, spec.Path, and spec.Name must be non-empty.
// An error is returned for any invariant violation.
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

	return n, nil
}
