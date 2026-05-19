package domain

import (
	"testing"
)

// DoD 6: NewEdge produces deterministic ID for same (src, kind, tgt).
func TestNewEdge_DeterministicID(t *testing.T) {
	e1, err := NewEdge("nodeA", "nodeB", EdgeCalls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	e2, err := NewEdge("nodeA", "nodeB", EdgeCalls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e1.ID != e2.ID {
		t.Errorf("expected deterministic ID, got %q and %q", e1.ID, e2.ID)
	}
}

// ID should be 32 hex chars.
func TestNewEdge_IDLength(t *testing.T) {
	e, err := NewEdge("nodeA", "nodeB", EdgeCalls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(e.ID) != 32 {
		t.Errorf("expected ID length 32, got %d: %q", len(e.ID), e.ID)
	}
}

// Different (src, kind, tgt) should produce different IDs.
func TestNewEdge_DifferentInputsDifferentID(t *testing.T) {
	e1, _ := NewEdge("nodeA", "nodeB", EdgeCalls)
	e2, _ := NewEdge("nodeA", "nodeC", EdgeCalls)
	if e1.ID == e2.ID {
		t.Error("expected different IDs for different edges")
	}
}

// DoD 7: confidence != Unresolved sets resolved = true.
func TestNewEdge_ConfidenceSetResolved(t *testing.T) {
	e, err := NewEdge("nodeA", "nodeB", EdgeCalls, WithConfidence(Probable))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !e.Resolved {
		t.Error("expected resolved=true when confidence != Unresolved")
	}
	if e.Confidence != Probable {
		t.Errorf("expected Probable confidence, got %v", e.Confidence)
	}
}

// Default confidence is Unresolved and resolved is false.
func TestNewEdge_DefaultUnresolved(t *testing.T) {
	e, err := NewEdge("nodeA", "nodeB", EdgeCalls)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Resolved {
		t.Error("expected resolved=false for default (Unresolved) confidence")
	}
	if e.Confidence != Unresolved {
		t.Errorf("expected Unresolved confidence, got %v", e.Confidence)
	}
}

// Strong and Definite also set resolved.
func TestNewEdge_StrongAndDefiniteResolved(t *testing.T) {
	for _, c := range []Confidence{Strong, Definite} {
		e, err := NewEdge("src", "tgt", EdgeImports, WithConfidence(c))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !e.Resolved {
			t.Errorf("expected resolved=true for confidence %v", c)
		}
	}
}

// NewEdge with empty src returns error.
func TestNewEdge_EmptySrc(t *testing.T) {
	_, err := NewEdge("", "nodeB", EdgeCalls)
	if err == nil {
		t.Fatal("expected error for empty src, got nil")
		return
	}
}

// NewEdge with empty tgt returns error.
func TestNewEdge_EmptyTgt(t *testing.T) {
	_, err := NewEdge("nodeA", "", EdgeCalls)
	if err == nil {
		t.Fatal("expected error for empty tgt, got nil")
		return
	}
}

// WithSourceLine sets the source_line optional field.
func TestNewEdge_WithSourceLine(t *testing.T) {
	line := 42
	e, err := NewEdge("nodeA", "nodeB", EdgeContains, WithSourceLine(line))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.SourceLine == nil || *e.SourceLine != line {
		t.Errorf("SourceLine: got %v, want %d", e.SourceLine, line)
	}
}

// EdgeKind enum values.
func TestEdgeKindValues(t *testing.T) {
	kinds := []EdgeKind{EdgeCalls, EdgeImports, EdgeContains, EdgeTests, EdgeDependsOn, EdgeSimilarTo}
	if len(kinds) != 6 {
		t.Errorf("expected 6 EdgeKind values, got %d", len(kinds))
	}
}

// DoD 6: NewEdge accepts SIMILAR_TO + Unresolved without rejecting the new kind.
func TestNewEdge_SimilarToUnresolved(t *testing.T) {
	e, err := NewEdge("nodeA", "nodeB", EdgeSimilarTo, WithConfidence(Unresolved))
	if err != nil {
		t.Fatalf("NewEdge with EdgeSimilarTo + Unresolved: %v", err)
	}
	if e.Kind != EdgeSimilarTo {
		t.Errorf("Kind: got %q, want %q", e.Kind, EdgeSimilarTo)
	}
	if e.Confidence != Unresolved {
		t.Errorf("Confidence: got %v, want Unresolved", e.Confidence)
	}
	if e.Resolved {
		t.Error("Resolved must be false for Unresolved confidence")
	}
	if len(e.ID) != 32 {
		t.Errorf("expected ID length 32, got %d", len(e.ID))
	}
}

// Confidence ordering.
func TestConfidenceOrdering(t *testing.T) {
	if Unresolved >= Probable || Probable >= Strong || Strong >= Definite {
		t.Error("Confidence ordering invariant violated")
	}
}

// Required fields are set correctly.
func TestNewEdge_RequiredFields(t *testing.T) {
	e, err := NewEdge("src123", "tgt456", EdgeTests)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(e.Src) != "src123" {
		t.Errorf("Src: got %q, want %q", e.Src, "src123")
	}
	if string(e.Tgt) != "tgt456" {
		t.Errorf("Tgt: got %q, want %q", e.Tgt, "tgt456")
	}
	if e.Kind != EdgeTests {
		t.Errorf("Kind: got %q, want %q", e.Kind, EdgeTests)
	}
}
