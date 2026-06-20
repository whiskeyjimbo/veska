// SPDX-License-Identifier: AGPL-3.0-only

package protocol

import "testing"

// TestDegradedReasonWireValues pins the JSON wire-contract strings. These
// values are part of the MCP response contract consumed by agents; a silent
// edit here would break clients.
func TestDegradedReasonWireValues(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ChainedSelectorsUnresolved", DegradedReasonChainedSelectorsUnresolved, "chained_selectors_unresolved"},
		{"ExternalCalleesOnly", DegradedReasonExternalCalleesOnly, "external_callees_only"},
		{"IndexingInProgress", DegradedReasonIndexingInProgress, "indexing_in_progress"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
