// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func TestNodeToDTO_SummaryStoredWins(t *testing.T) {
	n, err := domain.NewNode(
		domain.NodeSpec{ID: "s1", Path: "foo.go", Name: "Foo", Kind: domain.KindFunction},
		domain.WithSignature("func Foo() error"),
		domain.WithShortSummary("does the foo thing"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeToDTO(n).Summary; got != "does the foo thing" {
		t.Fatalf("stored summary should win, got %q", got)
	}
}

func TestNodeToDTO_SummaryFallsBackToHeuristic(t *testing.T) {
	// No stored summary: the DTO carries the heuristic so the field is always
	// populated.
	withSig, _ := domain.NewNode(
		domain.NodeSpec{ID: "s1", Path: "foo.go", Name: "Foo", Kind: domain.KindFunction},
		domain.WithSignature("func Foo() error"),
	)
	if got := nodeToDTO(withSig).Summary; got != "func Foo() error" {
		t.Fatalf("with signature: got %q, want signature heuristic", got)
	}

	noSig, _ := domain.NewNode(
		domain.NodeSpec{ID: "s2", Path: "foo.go", Name: "Bar", Kind: domain.KindType},
	)
	if got := nodeToDTO(noSig).Summary; got != "type Bar" {
		t.Fatalf("no signature: got %q, want %q", got, "type Bar")
	}
}
