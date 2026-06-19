// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import "testing"

// callers/callees aliases for the --direction flag must
// normalize to the canonical in/out enum the daemon expects.
func TestNormalizeDirection(t *testing.T) {
	cases := map[string]string{
		"callers": "in",
		"callees": "out",
		"in":      "in",
		"out":     "out",
		"both":    "both",
		"":        "",
		"bogus":   "bogus",
	}
	for in, want := range cases {
		if got := NormalizeDirection(in); got != want {
			t.Errorf("NormalizeDirection(%q) = %q, want %q", in, got, want)
		}
	}
}
