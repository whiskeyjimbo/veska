// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package layercheck_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/tools/lint/layercheck"
)

// TestNoViolationsInCurrentCodebase is an integration test that runs the full
// layer check against the actual internal/ package tree and asserts zero violations.
func TestNoViolationsInCurrentCodebase(t *testing.T) {
	t.Parallel()

	// Run the checker against the module root (two levels up from tools/lint/layercheck).
	violations, err := layercheck.CheckDir("../../..")
	if err != nil {
		t.Fatalf("CheckDir failed: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("found %d layer violation(s) in the current codebase:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s", v)
		}
	}
}
