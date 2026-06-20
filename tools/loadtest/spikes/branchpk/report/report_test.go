// SPDX-License-Identifier: AGPL-3.0-only

package report_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/report"
)

func makeInputs(nodeP95, edgesP95, diskGiB float64, linearGrowth bool) report.SpikeInputs {
	const gib = 1 << 30
	return report.SpikeInputs{
		Load: report.LoadMetrics{
			OverlapPct:  10,
			Branches:    50,
			Symbols:     100_000,
			NodeRows:    50 * 100_000,
			EdgeRows:    50 * 100_000,
			FindingRows: 50 * 1_000,
			DBBytes:     int64(diskGiB * float64(gib)),
		},
		Bench: report.BenchResult{
			OverlapPct:    10,
			Branches:      50,
			Symbols:       100_000,
			NodeBudgetMs:  25.0,
			EdgesBudgetMs: 100.0,
			NodeLatency:   report.LatencyStats{P95Ms: nodeP95},
			EdgesLatency:  report.LatencyStats{P95Ms: edgesP95},
		},
		GC: report.GCSweepResult{
			BranchesBefore: 50,
			BranchesAfter:  40,
			WallMs:         500,
		},
		LinearGrowthConfirmed: linearGrowth,
	}
}

func TestVerdictGreen(t *testing.T) {
	inputs := makeInputs(15, 80, 4.0, true)
	v := report.AssignVerdict(inputs)
	if v != report.VerdictGreen {
		t.Errorf("expected green, got %s", v)
	}
}

func TestVerdictYellow(t *testing.T) {
	// node p95=40ms (≤2×25ms budget=50ms), disk=8GiB (≤2×5GiB=10GiB)
	inputs := makeInputs(40, 80, 8.0, true)
	v := report.AssignVerdict(inputs)
	if v != report.VerdictYellow {
		t.Errorf("expected yellow, got %s", v)
	}
}

func TestVerdictRed(t *testing.T) {
	// node p95=60ms (>2×25ms budget=50ms)
	inputs := makeInputs(60, 80, 4.0, true)
	v := report.AssignVerdict(inputs)
	if v != report.VerdictRed {
		t.Errorf("expected red, got %s", v)
	}
}

func TestVerdictRedNonLinear(t *testing.T) {
	// super-linear growth → red
	inputs := makeInputs(15, 80, 4.0, false)
	v := report.AssignVerdict(inputs)
	if v != report.VerdictRed {
		t.Errorf("expected red for non-linear growth, got %s", v)
	}
}

func TestRenderMarkdown(t *testing.T) {
	inputs := makeInputs(15, 80, 4.0, true)
	v := report.AssignVerdict(inputs)
	md := report.RenderMarkdown(inputs, v)

	if len(md) == 0 {
		t.Fatal("RenderMarkdown returned empty string")
	}

	mustContain := []string{
		"green",   // verdict
		"15",      // node p95
		"80",      // edges p95
		"4",       // disk GiB
		"GC",      // GC section
		"SQLite",  // SQLite version citation
		"50",      // branches (population assumptions)
		"100000",  // symbols (population assumptions)
		"RESULTS", // title
	}
	for _, want := range mustContain {
		if !strings.Contains(md, want) {
			t.Errorf("RenderMarkdown output missing %q\nFull output:\n%s", want, md)
		}
	}
}
