// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// lowConfidenceTop fires only when the best non-chunk hit's absolute RRF score
// sits below the calibrated floor, the precise-logic-miss signature that drives
// the re-query spiral.
func TestLowConfidenceTop(t *testing.T) {
	below := search.WeakTopAbsolute - 0.001
	atOrAbove := search.WeakTopAbsolute + 0.001

	cases := []struct {
		name    string
		results []search.Result
		want    bool
	}{
		{"empty is not low-confidence", nil, false},
		{
			"strong top does not fire",
			[]search.Result{{NodeID: "a", Score: atOrAbove, Kind: "function"}},
			false,
		},
		{
			"weak top fires",
			[]search.Result{{NodeID: "a", Score: below, Kind: "function"}},
			true,
		},
		{
			"chunk-only top is ignored, so weak chunk does not fire",
			[]search.Result{{NodeID: "c", Score: below, Kind: string(domain.KindChunk)}},
			false,
		},
		{
			"strong non-chunk beats a weak chunk - does not fire",
			[]search.Result{
				{NodeID: "c", Score: below, Kind: string(domain.KindChunk)},
				{NodeID: "a", Score: atOrAbove, Kind: "function"},
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lowConfidenceTop(tc.results); got != tc.want {
				t.Fatalf("lowConfidenceTop = %v, want %v", got, tc.want)
			}
		})
	}
}
