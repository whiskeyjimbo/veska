// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
)

// filterBlastNoise must drop container/chunk kinds and keep
// every symbol-level kind, preserving order.
func TestFilterBlastNoise(t *testing.T) {
	in := []blastradius.Entry{
		{NodeID: "n1", Kind: "function"},
		{NodeID: "n2", Kind: "chunk"}, // noise
		{NodeID: "n3", Kind: "method"},
		{NodeID: "n4", Kind: "package"}, // noise
		{NodeID: "n5", Kind: "struct"},
		{NodeID: "n6", Kind: "module"}, // noise
		{NodeID: "n7", Kind: "file"},   // noise
		{NodeID: "n8", Kind: "variable"},
	}
	got := filterBlastNoise(in)
	wantIDs := []string{"n1", "n3", "n5", "n8"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, w := range wantIDs {
		if got[i].NodeID != w {
			t.Errorf("entry[%d] = %q, want %q (order must be preserved)", i, got[i].NodeID, w)
		}
	}
}

// Empty / all-noise inputs return an empty (non-nil-panic) slice.
func TestFilterBlastNoise_AllNoise(t *testing.T) {
	in := []blastradius.Entry{{Kind: "chunk"}, {Kind: "package"}}
	if got := filterBlastNoise(in); len(got) != 0 {
		t.Fatalf("all-noise input must yield 0 entries; got %+v", got)
	}
}
