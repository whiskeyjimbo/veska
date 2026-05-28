package main

import "testing"

// solov2-5out: callers/callees aliases for the --direction flag must
// normalise to the canonical in/out enum the daemon expects.
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
		if got := normalizeDirection(in); got != want {
			t.Errorf("normalizeDirection(%q) = %q, want %q", in, got, want)
		}
	}
}
