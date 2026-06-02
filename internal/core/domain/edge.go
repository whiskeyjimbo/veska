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
	// EdgeRoutes connects a router/mux node to the handler a framework
	// registration call binds to a path (gin/echo `router.GET("/p", h)`).
	// Reserved alongside KindRoute as the framework-aware vocabulary; the
	// cobra command tree uses CONTAINS (a command literally contains its
	// subcommands), so ROUTES emission lands with the route pass .
	EdgeRoutes EdgeKind = "ROUTES"
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
	EdgeRoutes:    {},
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
	// Score is the strength of the relationship, currently populated only for
	// EdgeSimilarTo edges by the auto-link pipeline: the vector-similarity
	// Hit.Score (1/(1+L2^2) space — see internal/application/autolink), where
	// higher means more similar. nil means "no score recorded" (every
	// non-SIMILAR_TO edge, plus legacy SIMILAR_TO rows promoted before the
	// score column existed). Near-duplicate detection thresholds on this.
	Score *float32
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

// WithScore sets the optional relationship-strength score (see Edge.Score).
// Used by the auto-link pipeline to record the vector-similarity score on a
// SIMILAR_TO edge so near-duplicate detection can threshold it.
func WithScore(score float32) EdgeOption {
	return func(e *Edge) error {
		e.Score = &score
		return nil
	}
}

// edgeID computes the deterministic edge identifier.
func edgeID(src NodeID, kind EdgeKind, tgt NodeID) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s", src, kind, tgt)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// EdgeSpec carries the required fields of an Edge. It groups the constructor's
// positional arguments into a named struct so the two adjacent same-typed
// NodeID fields (Src/Tgt) cannot be transposed at a call site, mirroring
// NodeSpec/FindingSpec. Optional fields (confidence, source line) are still
// supplied via EdgeOption.
type EdgeSpec struct {
	Src  NodeID
	Tgt  NodeID
	Kind EdgeKind
}

// NewEdge constructs an Edge from spec, validates invariants, and applies
// functional options. spec.Src and spec.Tgt must be non-empty and spec.Kind
// must be a recognised EdgeKind.
func NewEdge(spec EdgeSpec, opts ...EdgeOption) (*Edge, error) {
	if spec.Src == "" {
		return nil, errors.New("edge: src must not be empty")
	}
	if spec.Tgt == "" {
		return nil, errors.New("edge: tgt must not be empty")
	}
	if _, ok := validEdgeKinds[spec.Kind]; !ok {
		return nil, errors.New("edge: invalid kind")
	}

	e := &Edge{
		ID:   edgeID(spec.Src, spec.Kind, spec.Tgt),
		Src:  spec.Src,
		Tgt:  spec.Tgt,
		Kind: spec.Kind,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, err
		}
	}

	return e, nil
}
