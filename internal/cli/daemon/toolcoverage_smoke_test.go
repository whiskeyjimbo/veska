// SPDX-License-Identifier: AGPL-3.0-only

package daemon

// Harness substrate smoke test.
// Every TestToolCoverage leaf currently t.Skips, so the harness's two core APIs
// Call and ResolveID - would otherwise never execute. This test exercises both
// against the real indexed fixture so the 40 downstream beads build on a proven
// substrate, and it validates the EXACT template the TestToolCoverage doc gives
// them (eng_get_node with a ResolveID'd node_id). It is a harness self-test, not
// a per-tool coverage assertion: it does not assert tool semantics against the
// manifest - only that the substrate dispatches a real handler against real data.

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp/coverage"
)

func TestHarnessSmoke(t *testing.T) {
	h := newHarness(t)

	// ResolveID must align with what indexRepo actually walked: the resolved ID
	// has to be a real row in nodes. This locks the ModuleRoot↔indexing path
	// against cwd / symlink / locator drift.
	id := h.ResolveID(coverage.BetaRepoID, coverage.NodeKey{
		Path: "main.go", Kind: domain.KindFunction, Name: "main",
	})
	var n int
	if err := h.pools.ReadDB.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE node_id = ?`, string(id),
	).Scan(&n); err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	if n == 0 {
		t.Fatalf("ResolveID produced %s, not present in nodes table", id)
	}

	// Call must dispatch a real handler against the indexed data. Mirrors the
	// template in the TestToolCoverage doc comment, so a wrong param name here
	// would catch a broken example before 40 authors copy it.
	res, rpcErr := h.Call("eng_get_node", map[string]any{"node_id": string(id)})
	if rpcErr != nil {
		t.Fatalf("eng_get_node Call: %v", rpcErr)
	}
	if res == nil {
		t.Fatal("eng_get_node returned a nil result for a known node")
	}
}
