// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package tokenize_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/tokenize"
)

func TestSymbol_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		// want is the set of tokens that MUST appear in the output (order-agnostic).
		want []string
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "camelCase",
			in:   "closeFinding",
			want: []string{"closeFinding", "close", "Finding"},
		},
		{
			name: "PascalCase",
			in:   "CloseFinding",
			want: []string{"CloseFinding", "Close", "Finding"},
		},
		{
			name: "snake_case",
			in:   "close_finding",
			want: []string{"close", "finding"},
		},
		{
			name: "double_colon",
			in:   "pkg::api::closeFinding",
			want: []string{"pkg", "api", "closeFinding", "close", "Finding"},
		},
		{
			name: "dot path",
			in:   "pkg.api.closeFinding",
			want: []string{"pkg", "api", "closeFinding", "close", "Finding"},
		},
		{
			name: "slash path",
			in:   "pkg/api/closeFinding",
			want: []string{"pkg", "api", "closeFinding", "close", "Finding"},
		},
		{
			name: "acronym mid-identifier",
			in:   "HTTPServer",
			want: []string{"HTTPServer", "HTTP", "Server"},
		},
		{
			name: "digits split",
			in:   "parseURL2Path",
			want: []string{"parseURL2Path", "parse", "URL", "2", "Path"},
		},
		{
			name: "all lower no split",
			in:   "closefinding",
			want: []string{"closefinding"},
		},
		{
			name: "kind + symbol + name combined (DoD example)",
			in:   "function pkg/api closeFinding",
			want: []string{"function", "pkg", "api", "closeFinding", "close", "Finding"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tokenize.Symbol(tc.in)
			// No leading/trailing whitespace; no consecutive spaces.
			if got != strings.TrimSpace(got) {
				t.Errorf("output has surrounding whitespace: %q", got)
			}
			if strings.Contains(got, "  ") {
				t.Errorf("output has consecutive spaces: %q", got)
			}
			if tc.in == "" {
				if got != "" {
					t.Errorf("empty input must yield empty output, got %q", got)
				}
				return
			}
			toks := strings.Fields(got)
			set := make(map[string]bool, len(toks))
			for _, tok := range toks {
				set[tok] = true
			}
			for _, w := range tc.want {
				if !set[w] {
					t.Errorf("input %q: token %q missing from %q", tc.in, w, got)
				}
			}
		})
	}
}
