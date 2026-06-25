// SPDX-License-Identifier: AGPL-3.0-only

package domain

import (
	"testing"
)

func TestNewEdge_DeterministicID(t *testing.T) {
	e1, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeCalls})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	e2, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeCalls})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e1.ID != e2.ID {
		t.Errorf("expected deterministic ID, got %q and %q", e1.ID, e2.ID)
	}
}

func TestNewEdge_IDLength(t *testing.T) {
	e, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeCalls})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(e.ID) != 32 {
		t.Errorf("expected ID length 32, got %d: %q", len(e.ID), e.ID)
	}
}

func TestNewEdge_DifferentInputsDifferentID(t *testing.T) {
	e1, _ := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeCalls})
	e2, _ := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeC", Kind: EdgeCalls})
	if e1.ID == e2.ID {
		t.Error("expected different IDs for different edges")
	}
}

func TestNewEdge_ConfidenceSetResolved(t *testing.T) {
	e, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeCalls}, WithConfidence(Probable))
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

func TestNewEdge_DefaultUnresolved(t *testing.T) {
	e, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeCalls})
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

func TestNewEdge_StrongAndDefiniteResolved(t *testing.T) {
	for _, c := range []Confidence{Strong, Definite} {
		e, err := NewEdge(EdgeSpec{Src: "src", Tgt: "tgt", Kind: EdgeImports}, WithConfidence(c))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !e.Resolved {
			t.Errorf("expected resolved=true for confidence %v", c)
		}
	}
}

func TestNewEdge_EmptySrc(t *testing.T) {
	_, err := NewEdge(EdgeSpec{Src: "", Tgt: "nodeB", Kind: EdgeCalls})
	if err == nil {
		t.Fatal("expected error for empty src, got nil")
		return
	}
}

func TestNewEdge_EmptyTgt(t *testing.T) {
	_, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "", Kind: EdgeCalls})
	if err == nil {
		t.Fatal("expected error for empty tgt, got nil")
		return
	}
}

func TestNewEdge_WithSourceLine(t *testing.T) {
	line := 42
	e, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeContains}, WithSourceLine(line))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.SourceLine == nil || *e.SourceLine != line {
		t.Errorf("SourceLine: got %v, want %d", e.SourceLine, line)
	}
}

func TestEdgeKindValues(t *testing.T) {
	kinds := []EdgeKind{EdgeCalls, EdgeImports, EdgeContains, EdgeTests, EdgeDependsOn, EdgeSimilarTo, EdgeRoutes, EdgeImplements, EdgeEmbeds}
	if len(kinds) != 9 {
		t.Errorf("expected 9 EdgeKind values, got %d", len(kinds))
	}
}

func TestNewEdge_SimilarToUnresolved(t *testing.T) {
	e, err := NewEdge(EdgeSpec{Src: "nodeA", Tgt: "nodeB", Kind: EdgeSimilarTo}, WithConfidence(Unresolved))
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

func TestNewEdge_KindValidation(t *testing.T) {
	valid := []EdgeKind{EdgeCalls, EdgeImports, EdgeContains, EdgeTests, EdgeDependsOn, EdgeSimilarTo, EdgeRoutes, EdgeImplements, EdgeEmbeds}
	if len(valid) != 9 {
		t.Fatalf("expected 9 valid EdgeKinds, listed %d", len(valid))
	}
	for _, k := range valid {
		t.Run(string(k), func(t *testing.T) {
			_, err := NewEdge(EdgeSpec{Src: "src", Tgt: "tgt", Kind: k})
			if err != nil {
				t.Fatalf("valid kind %q rejected: %v", k, err)
			}
		})
	}
	if _, err := NewEdge(EdgeSpec{Src: "src", Tgt: "tgt", Kind: EdgeKind("bogus")}); err == nil {
		t.Fatal("expected error for unknown EdgeKind, got nil")
	}
}

func TestWithConfidence_RangeValidation(t *testing.T) {
	cases := []struct {
		c        Confidence
		resolved bool
	}{
		{Unresolved, false},
		{Probable, true},
		{Strong, true},
		{Definite, true},
	}
	for _, tc := range cases {
		e, err := NewEdge(EdgeSpec{Src: "src", Tgt: "tgt", Kind: EdgeCalls}, WithConfidence(tc.c))
		if err != nil {
			t.Fatalf("valid confidence %v rejected: %v", tc.c, err)
		}
		if e.Resolved != tc.resolved {
			t.Errorf("confidence %v: Resolved=%v, want %v", tc.c, e.Resolved, tc.resolved)
		}
	}
	for _, bad := range []Confidence{Confidence(99), Confidence(-1)} {
		if _, err := NewEdge(EdgeSpec{Src: "src", Tgt: "tgt", Kind: EdgeCalls}, WithConfidence(bad)); err == nil {
			t.Fatalf("expected error for out-of-range confidence %v, got nil", bad)
		}
	}
}

func TestConfidenceOrdering(t *testing.T) {
	if Unresolved >= Probable || Probable >= Strong || Strong >= Definite {
		t.Error("Confidence ordering invariant violated")
	}
}

func TestNewEdge_RequiredFields(t *testing.T) {
	e, err := NewEdge(EdgeSpec{Src: "src123", Tgt: "tgt456", Kind: EdgeTests})
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
