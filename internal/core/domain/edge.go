// SPDX-License-Identifier: AGPL-3.0-only

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
	// EdgeSimilarTo represents a proposed semantic similarity edge emitted by the
	// auto-link pipeline. These are created with Confidence=Unresolved and paired
	// with a 'semantic' finding for review.
	EdgeSimilarTo EdgeKind = "SIMILAR_TO"
	// EdgeRoutes connects a router node to the HTTP handler bound by a framework
	// registration call. The cobra command tree uses CONTAINS for subcommand
	// relationships.
	EdgeRoutes EdgeKind = "ROUTES"
	// EdgeImplements connects a concrete type to an interface it satisfies
	// (Go method-set satisfaction). Resolved at promotion from the parsed
	// method sets; carries a Confidence reflecting how certain the match is.
	EdgeImplements EdgeKind = "IMPLEMENTS"
	// EdgeEmbeds connects a struct or interface to a type it embeds (an
	// unnamed field / embedded interface). Methods of the embedded type are
	// promoted onto the embedder, which feeds IMPLEMENTS resolution.
	EdgeEmbeds EdgeKind = "EMBEDS"
)

// validEdgeKinds defines the closed set of valid EdgeKind values enforced by NewEdge.
var validEdgeKinds = map[EdgeKind]struct{}{
	EdgeCalls:      {},
	EdgeImports:    {},
	EdgeContains:   {},
	EdgeTests:      {},
	EdgeDependsOn:  {},
	EdgeSimilarTo:  {},
	EdgeRoutes:     {},
	EdgeImplements: {},
	EdgeEmbeds:     {},
}

// advisoryEdgeKinds are non-structural edges: they record a suggested or
// semantic relationship for review, not an actual dependency in the code.
// Impact and reachability traversal (blast radius, context packs, entry-point
// detection) must exclude them - a SIMILAR_TO edge between two look-alike
// symbols would otherwise bridge unrelated subgraphs and inflate the result.
// Everything not listed here is structural and IS traversed, so new structural
// edge kinds are included automatically.
var advisoryEdgeKinds = map[EdgeKind]struct{}{
	EdgeSimilarTo: {},
}

// IsAdvisory reports whether the edge kind is advisory (semantic/suggested)
// rather than structural. Advisory edges are excluded from impact and
// reachability traversal.
func (k EdgeKind) IsAdvisory() bool {
	_, ok := advisoryEdgeKinds[k]
	return ok
}

// AdvisoryEdgeKinds returns the advisory (non-structural) edge kinds. Adapters
// building a "structural neighbors" query exclude these; the order is stable
// for deterministic SQL.
func AdvisoryEdgeKinds() []EdgeKind {
	return []EdgeKind{EdgeSimilarTo}
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
	// Score indicates the strength of the relationship, populated only for
	// EdgeSimilarTo edges. It represents vector similarity where higher is
	// more similar, and is used to threshold near-duplicates.
	Score *float32
}

type EdgeOption func(*Edge) error

// WithConfidence sets the edge confidence and derives the corresponding resolved flag.
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

func WithSourceLine(line int) EdgeOption {
	return func(e *Edge) error {
		e.SourceLine = &line
		return nil
	}
}

// WithScore sets the relationship-strength score for similarity thresholding.
func WithScore(score float32) EdgeOption {
	return func(e *Edge) error {
		e.Score = &score
		return nil
	}
}

// edgeID computes a deterministic SHA-256 edge identifier from its source, target, and kind.
func edgeID(src NodeID, kind EdgeKind, tgt NodeID) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s", src, kind, tgt)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// EdgeSpec groups the required fields of an Edge into a struct to prevent
// transposing the adjacent same-typed NodeID fields (Src/Tgt) at construction call sites.
type EdgeSpec struct {
	Src  NodeID
	Tgt  NodeID
	Kind EdgeKind
}

// NewEdge constructs a validated Edge, applying functional options and verifying
// that Src, Tgt, and Kind are valid.
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
