// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

// Isolation proof for the tool-coverage harness.
// The ~11 mutating tools (add_repo, remove_repo, set/remove_repo_alias,
// promote/reindex_repo, close/reopen/suppress_finding, close_suppression,
// set_active_task) make a shared DB order-dependent. newHarness defends against
// that by building a fresh isolated DB per call. This test proves it: a mutation
// performed through one harness instance is invisible to a second fresh instance.

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp/coverage"
)

// TestHarnessIsolation_MutationDoesNotLeak inserts a repo alias directly into
// one harness's DB, then asserts a second freshly-built harness does not see it.
// The alias write stands in for any mutating tool: if the DB were shared the
// second instance would observe the row.
func TestHarnessIsolation_MutationDoesNotLeak(t *testing.T) {
	const probe = "isolation-probe-alias"

	h1 := newHarness(t)
	h1.execSeed(`INSERT INTO repo_aliases (name, repo_id) VALUES (?, ?)`,
		probe, coverage.AlphaRepoID)
	if !aliasExists(t, h1, probe) {
		t.Fatalf("precondition: alias %q not visible in its own harness", probe)
	}

	h2 := newHarness(t)
	if aliasExists(t, h2, probe) {
		t.Errorf("mutation leaked: alias %q from h1 is visible in fresh harness h2", probe)
	}
}

func aliasExists(t *testing.T, h *toolHarness, name string) bool {
	t.Helper()
	var n int
	if err := h.pools.ReadDB.QueryRow(
		`SELECT COUNT(*) FROM repo_aliases WHERE name = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("query alias: %v", err)
	}
	return n > 0
}
