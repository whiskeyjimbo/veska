// SPDX-License-Identifier: AGPL-3.0-only

package report_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/report"
)

func greenInputs() report.SpikeInputs {
	return report.SpikeInputs{
		Bench: report.BenchResult{
			Pops: []report.PopBench{
				{
					Population: 50_000,
					K:          10,
					Warm:       report.LatencyStats{P95Ms: 80.0},
				},
				{
					Population: 1_000_000,
					K:          10,
					Warm:       report.LatencyStats{P95Ms: 150.0},
				},
			},
			Vec0Ceiling:      300_000,
			CeilingReason:    "none",
			SqliteVecVersion: "v0.1.6",
			SqliteVersion:    "3.45.1",
			Platform:         "linux/amd64",
		},
		Recall: []report.RecallResult{
			{Population: 50_000, RecallAt10: 0.97, RecallAt50: 0.98, HoldOutSize: 100},
			{Population: 1_000_000, RecallAt10: 0.90, RecallAt50: 0.92, HoldOutSize: 100},
		},
		Load: []report.LoadMetric{
			{Population: 50_000, LoadWallMs: 1000, DiskBytes: 200_000_000, PeakRSSBytes: 512_000_000},
			{Population: 1_000_000, LoadWallMs: 30_000, DiskBytes: 4_000_000_000, PeakRSSBytes: 1_500_000_000},
		},
	}
}

func TestVerdictGreen(t *testing.T) {
	inputs := greenInputs()
	v := report.AssignVerdict(inputs)
	if v.Bucket != "green" {
		t.Errorf("expected green, got %q; reasons: %v", v.Bucket, v.Reasons)
	}
}

func TestVerdictYellow(t *testing.T) {
	// 50k passes, 1M p95=300ms + recall=0.80, ceiling=300k
	inputs := greenInputs()
	inputs.Bench.Pops[1].Warm.P95Ms = 300.0
	inputs.Recall[1].RecallAt10 = 0.80
	v := report.AssignVerdict(inputs)
	if v.Bucket != "yellow" {
		t.Errorf("expected yellow, got %q; reasons: %v", v.Bucket, v.Reasons)
	}
}

func TestVerdictRedQuality(t *testing.T) {
	// 50k p95=150ms → red-quality (misses 100ms gate)
	inputs := greenInputs()
	inputs.Bench.Pops[0].Warm.P95Ms = 150.0
	v := report.AssignVerdict(inputs)
	if v.Bucket != "red-quality" {
		t.Errorf("expected red-quality, got %q; reasons: %v", v.Bucket, v.Reasons)
	}
}

func TestVerdictRedCeiling(t *testing.T) {
	// ceiling=100k → red-ceiling regardless of latency
	inputs := greenInputs()
	inputs.Bench.Vec0Ceiling = 100_000
	v := report.AssignVerdict(inputs)
	if v.Bucket != "red-ceiling" {
		t.Errorf("expected red-ceiling, got %q; reasons: %v", v.Bucket, v.Reasons)
	}
}

func TestRenderMarkdown(t *testing.T) {
	inputs := greenInputs()
	v := report.AssignVerdict(inputs)
	md := report.RenderMarkdown(inputs, v)
	if md == "" {
		t.Fatal("RenderMarkdown returned empty string")
	}

	mustContain := []string{
		"80",     // 50k p95
		"150",    // 1M p95
		"0.97",   // 50k recall
		"0.90",   // 1M recall
		"300000", // ceiling
		"green",  // verdict bucket
		"v0.1.6", // sqlite-vec version
	}
	for _, needle := range mustContain {
		if !strings.Contains(md, needle) {
			t.Errorf("RenderMarkdown output missing %q", needle)
		}
	}
}
