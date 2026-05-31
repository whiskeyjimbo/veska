package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// EdgeKind is a closed enum of the structural relationships between nodes.
type EdgeKind string

const (
	EdgeCalls     EdgeKind = "CALLS"
	EdgeImports   EdgeKind = "IMPORTS"
	EdgeContains  EdgeKind = "CONTAINS"
	EdgeTests     EdgeKind = "TESTS"
	EdgeDependsOn EdgeKind = "DEPENDS_ON"
	// EdgeSimilarTo is the auto-link kind: a proposed semantic similarity edge
	// emitted by the auto-link pipeline (see internal/application/autolink).
	// These edges are written with Confidence=Unresolved and paired with a
	// source_layer='semantic' Finding for human review.
	EdgeSimilarTo EdgeKind = "SIMILAR_TO"
)

// validEdgeKinds is the closed set of recognised EdgeKind values. NewEdge
// rejects any kind outside this set, mirroring validNodeKinds in node.go.
var validEdgeKinds = map[EdgeKind]struct{}{
	EdgeCalls:     {},
	EdgeImports:   {},
	EdgeContains:  {},
	EdgeTests:     {},
	EdgeDependsOn: {},
	EdgeSimilarTo: {},
}

// Confidence is an ordered enum representing how certain an edge relationship is.
type Confidence int

const (
	Unresolved Confidence = iota
	Probable
	Strong
	Definite
)

// Edge is a directed relationship between two Nodes.
type Edge struct {
	// ID is a deterministic 32-hex-char identifier derived from (Src, Kind, Tgt).
	ID         string
	Src        NodeID
	Tgt        NodeID
	Kind       EdgeKind
	Confidence Confidence
	Resolved   bool
	SourceLine *int
}

// EdgeOption is a functional option applied during Edge construction.
type EdgeOption func(*Edge) error

// WithConfidence sets the confidence level and derives the resolved flag.
func WithConfidence(c Confidence) EdgeOption {
	return func(e *Edge) error {
		if c < Unresolved || c > Definite {
			return errors.New("edge: invalid confidence")
		}
		e.Confidence = c
		e.Resolved = c != Unresolved
		return nil
	}
}

// WithSourceLine sets the optional 1-indexed source line of the edge reference.
func WithSourceLine(line int) EdgeOption {
	return func(e *Edge) error {
		e.SourceLine = &line
		return nil
	}
}

// edgeID computes the deterministic edge identifier.
func edgeID(src NodeID, kind EdgeKind, tgt NodeID) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s", src, kind, tgt)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// NewEdge constructs an Edge, validates invariants, and applies functional options.
// src and tgt must be non-empty.
func NewEdge(src, tgt NodeID, kind EdgeKind, opts ...EdgeOption) (*Edge, error) {
	if src == "" {
		return nil, errors.New("edge: src must not be empty")
	}
	if tgt == "" {
		return nil, errors.New("edge: tgt must not be empty")
	}
	if _, ok := validEdgeKinds[kind]; !ok {
		return nil, errors.New("edge: invalid kind")
	}

	e := &Edge{
		ID:   edgeID(src, kind, tgt),
		Src:  src,
		Tgt:  tgt,
		Kind: kind,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, err
		}
	}

	return e, nil
}
