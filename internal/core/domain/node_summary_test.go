// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

import (
	"strings"
	"testing"
)

func TestWithShortSummary_RuneLimit(t *testing.T) {
	spec := NodeSpec{ID: "n1", Path: "x.go", Name: "Foo", Kind: KindFunction}

	ok := strings.Repeat("a", MaxShortSummaryRunes)
	n, err := NewNode(spec, WithShortSummary(ok))
	if err != nil {
		t.Fatalf("exactly %d runes should be allowed: %v", MaxShortSummaryRunes, err)
	}
	if n.ShortSummary == nil || *n.ShortSummary != ok {
		t.Fatalf("ShortSummary not set")
	}

	tooLong := strings.Repeat("b", MaxShortSummaryRunes+1)
	if _, err := NewNode(spec, WithShortSummary(tooLong)); err == nil {
		t.Fatalf("%d runes should be rejected", MaxShortSummaryRunes+1)
	}

	// Multi-byte runes are counted as runes, not bytes: 280 emoji (4 bytes
	// each) are allowed even though the byte length far exceeds 280.
	emoji := strings.Repeat("🚀", MaxShortSummaryRunes)
	if _, err := NewNode(spec, WithShortSummary(emoji)); err != nil {
		t.Fatalf("%d runes of multi-byte text should be allowed: %v", MaxShortSummaryRunes, err)
	}
}

func TestHeuristicSummary(t *testing.T) {
	sig := "func Foo(a int) error"
	withSig, _ := NewNode(NodeSpec{ID: "n1", Path: "x.go", Name: "Foo", Kind: KindFunction}, WithSignature(sig))
	if got := withSig.HeuristicSummary(); got != sig {
		t.Fatalf("with signature: got %q, want %q", got, sig)
	}

	noSig, _ := NewNode(NodeSpec{ID: "n2", Path: "x.go", Name: "Bar", Kind: KindType})
	if got := noSig.HeuristicSummary(); got != "type Bar" {
		t.Fatalf("no signature: got %q, want %q", got, "type Bar")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := TruncateRunes("hello", 10); got != "hello" {
		t.Fatalf("under limit: got %q", got)
	}
	if got := TruncateRunes("hello", 3); got != "hel" {
		t.Fatalf("over limit: got %q, want %q", got, "hel")
	}
	if got := TruncateRunes("🚀🚀🚀", 2); got != "🚀🚀" {
		t.Fatalf("multi-byte boundary: got %q", got)
	}
}
