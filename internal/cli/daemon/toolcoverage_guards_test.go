// SPDX-License-Identifier: AGPL-3.0-only

package daemon

// Guard tests for the tool-coverage skeleton:
//   TestToolCoverageCompleteness: the coverageTools table covers exactly the
//     live tool surface - the 37 production tools plus the 3 opt-in task tools,
//     and no more. This is what stops a newly-added MCP tool from slipping in
//     uncovered, and stops a removed tool from rotting in the table.
//   TestToolCoverageHarnessMatchesProduction: the harness's default registry
//     (no task opt-in) registers exactly the same tool names a fresh PRODUCTION
//     registry does. The production reference comes from newDaemon, NOT from a
//     second registerMCPTools call - comparing the harness registry to another
//     registerMCPTools output would be circular and prove nothing. It catches a
//     harness that forgot to wire ingester/promoter/reparser (which would drop
//     eng_promote_repo / eng_reindex_repo).

import (
	"sort"
	"testing"
)

// TestToolCoverageCompleteness asserts the table's tool set == harness default
// names (37) ∪ task opt-in names (3), with no missing and no extra entries, and
// that every table row is keyed correctly (parked task tools flagged task=true).
func TestToolCoverageCompleteness(t *testing.T) {
	t.Parallel()

	// Live surface: default (36) + task opt-in (3) = 39 distinct names.
	defNames := newHarness(t).Registry().Names()
	allNames := newHarness(t, WithTaskTools()).Registry().Names()

	taskOnly := setDiff(allNames, defNames)
	if len(taskOnly) != 3 {
		t.Fatalf("task opt-in added %d tools; want 3 (got %v)", len(taskOnly), taskOnly)
	}

	wantSet := toSet(allNames)

	// Table set, plus the task-flag correctness check.
	tableSet := map[string]bool{}
	taskFlag := map[string]bool{}
	for _, ct := range coverageTools() {
		if tableSet[ct.tool] {
			t.Errorf("duplicate table entry for tool %q", ct.tool)
		}
		tableSet[ct.tool] = true
		taskFlag[ct.tool] = ct.task
		if ct.bead == "" {
			t.Errorf("tool %q has no owning bead in the table", ct.tool)
		}
	}

	// Every live tool must be in the table.
	for name := range wantSet {
		if !tableSet[name] {
			t.Errorf("live tool %q is not covered by the coverageTools table", name)
		}
	}
	// Every table entry must be a live tool.
	for name := range tableSet {
		if !wantSet[name] {
			t.Errorf("table covers tool %q that is not in the live surface", name)
		}
	}
	// The 3 task-opt-in tools must be flagged task=true; nothing else may be.
	taskSet := toSet(taskOnly)
	for name, flagged := range taskFlag {
		if taskSet[name] != flagged {
			t.Errorf("tool %q task flag = %v; want %v (task-opt-in set: %v)",
				name, flagged, taskSet[name], taskOnly)
		}
	}

	if got := len(tableSet); got != 39 {
		t.Errorf("coverage table has %d tools; want 39", got)
	}
}

// TestToolCoverageHarnessMatchesProduction asserts the harness default registry
// names equal a fresh production daemon's registry names. newDaemon only BUILDS
// the daemon (Start spawns the goroutines); it is the independent reference.
func TestToolCoverageHarnessMatchesProduction(t *testing.T) {
	d, err := newDaemon(testConfig(t))
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	prod := d.mcpRegistry().Names()
	harness := newHarness(t).Registry().Names()

	if !equalStringSets(prod, harness) {
		t.Errorf("harness tool names diverge from production\n production-only: %v\n harness-only:   %v",
			setDiff(prod, harness), setDiff(harness, prod))
	}
}

// small set helpers

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

// setDiff returns the elements of a not present in b, sorted.
func setDiff(a, b []string) []string {
	bs := toSet(b)
	var out []string
	for _, v := range a {
		if !bs[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	return len(setDiff(a, b)) == 0 && len(setDiff(b, a)) == 0
}
