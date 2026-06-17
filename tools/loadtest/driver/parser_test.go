// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build loadtest

package main

import (
	"testing"
)

func TestParseColdScan(t *testing.T) {
	fixture := `# Cold-Scan Benchmark - GoParser

Generated: 2026-05-13
Platform: linux amd64
Files: 1000
Total LOC: ~119k
Gate: <60s total elapsed

| Metric | Value | Gate |
|--------|-------|------|
| Total elapsed | 1.616s | <60s |
| Per-file p95 | 3.04ms | - |
| Files/sec | 619 | - |

Gate: PASS
`
	val, pass, err := parseColdScan(fixture)
	if err != nil {
		t.Fatalf("parseColdScan error: %v", err)
	}
	if val != "1.616s" {
		t.Errorf("want 1.616s, got %q", val)
	}
	if !pass {
		t.Error("want PASS")
	}
}

func TestParseColdScan_Fail(t *testing.T) {
	fixture := `| Total elapsed | 72.300s | <60s |

Gate: FAIL`
	_, pass, err := parseColdScan(fixture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pass {
		t.Error("expected FAIL")
	}
}

func TestParseColdScan_Missing(t *testing.T) {
	_, _, err := parseColdScan("no relevant lines here")
	if err == nil {
		t.Error("expected error for missing metric")
	}
}

func TestParseHookBench(t *testing.T) {
	fixture := `# Hook p95 Benchmark - sendSeal round-trip

| Metric | Value | Gate |
|--------|-------|------|
| p95 latency | 0.116ms | ≤100ms |
| p99 latency | 0.208ms | - |

Gate: PASS (p95 ≤ 100ms)
`
	val, pass, err := parseHookBench(fixture)
	if err != nil {
		t.Fatalf("parseHookBench error: %v", err)
	}
	if val != "0.116ms" {
		t.Errorf("want 0.116ms, got %q", val)
	}
	if !pass {
		t.Error("want PASS")
	}
}

func TestParseHookBench_Missing(t *testing.T) {
	_, _, err := parseHookBench("no relevant lines")
	if err == nil {
		t.Error("expected error for missing metric")
	}
}

func TestParseVectorBench(t *testing.T) {
	fixture := `## Results

| Population | Recall@10 | P95 (ms) | Recall Floor | P95 Budget | Pass |
|-----------|-----------|----------|-------------|-----------|------|
| 50k | 0.9870 (≥0.95) | 1.90 (≤100) | 0.95 | 100 | ✓ |
| 250k | 0.9540 (≥0.85) | 4.28 (≤150) | 0.85 | 150 | ✓ |
`
	val, pass, err := parseVectorBench(fixture)
	if err != nil {
		t.Fatalf("parseVectorBench error: %v", err)
	}
	if val != "1.90ms" {
		t.Errorf("want 1.90ms, got %q", val)
	}
	if !pass {
		t.Error("want PASS")
	}
}

func TestParseVectorBench_Missing(t *testing.T) {
	_, _, err := parseVectorBench("no relevant lines")
	if err == nil {
		t.Error("expected error for missing metric")
	}
}

func TestParseCIGates(t *testing.T) {
	fixture := `# CI Gates

| Gate | Verdict |
|------|---------|
| race tests | PASS |
| golangci-lint | PASS |
| layercheck | PASS |
`
	race, lint, err := parseCIGates(fixture)
	if err != nil {
		t.Fatalf("parseCIGates error: %v", err)
	}
	if !race {
		t.Error("want race PASS")
	}
	if !lint {
		t.Error("want lint PASS")
	}
}

func TestParseCIGates_Missing(t *testing.T) {
	_, _, err := parseCIGates("no relevant lines")
	if err == nil {
		t.Error("expected error for missing gate lines")
	}
}

func TestParseMCPLatencyBench(t *testing.T) {
	fixture := `# MCP Latency Benchmark - find_symbol warm p95

Generated: 2026-05-13
Platform: linux amd64
Nodes seeded: 50000

| Metric | Value | Gate | Verdict |
|--------|-------|------|---------|
| find_symbol warm p95 | 0.042ms | <50ms | PASS |

Gate: PASS (p95 < 50ms)
`
	ms, ok := parseMCPLatencyBench(fixture)
	if !ok {
		t.Fatal("parseMCPLatencyBench: expected ok=true")
	}
	if ms < 0 || ms > 50 {
		t.Errorf("expected ms in [0,50), got %f", ms)
	}
}

func TestParseMCPLatencyBench_Fail(t *testing.T) {
	fixture := `| find_symbol warm p95 | 75.3ms | <50ms | FAIL |`
	ms, ok := parseMCPLatencyBench(fixture)
	if !ok {
		t.Fatal("parseMCPLatencyBench: expected ok=true (can parse even FAIL result)")
	}
	if ms != 75.3 {
		t.Errorf("want 75.3, got %f", ms)
	}
}

func TestParseMCPLatencyBench_Missing(t *testing.T) {
	_, ok := parseMCPLatencyBench("no relevant lines here")
	if ok {
		t.Error("expected ok=false for missing metric line")
	}
}

func TestParseMultiBranchBench_Pass(t *testing.T) {
	fixture := `# Multi-Branch Bench

| Metric | Value | Gate | Verdict |
|--------|-------|------|---------|
| Daemon RSS | 342mb | ≤2GiB | PASS |
| Promotion 50k p95 | 0.82s | <5s | PASS |
`
	rssBytes, p95Secs, ok := parseMultiBranchBench(fixture)
	if !ok {
		t.Fatal("expected ok=true")
	}
	wantRSS := int64(342 * 1024 * 1024)
	if rssBytes != wantRSS {
		t.Errorf("want rssBytes=%d, got %d", wantRSS, rssBytes)
	}
	if p95Secs < 0.81 || p95Secs > 0.83 {
		t.Errorf("want p95Secs≈0.82, got %f", p95Secs)
	}
}

func TestParseMultiBranchBench_GiB(t *testing.T) {
	fixture := `| Daemon RSS | 1.50GiB | ≤2GiB | PASS |
| Promotion 50k p95 | 3.14s | <5s | PASS |
`
	rssBytes, p95Secs, ok := parseMultiBranchBench(fixture)
	if !ok {
		t.Fatal("expected ok=true")
	}
	wantRSS := int64(1.50 * 1024 * 1024 * 1024)
	if rssBytes != wantRSS {
		t.Errorf("want rssBytes=%d, got %d", wantRSS, rssBytes)
	}
	if p95Secs < 3.13 || p95Secs > 3.15 {
		t.Errorf("want p95Secs≈3.14, got %f", p95Secs)
	}
}

func TestParseMultiBranchBench_Missing(t *testing.T) {
	_, _, ok := parseMultiBranchBench("no relevant lines here")
	if ok {
		t.Error("expected ok=false for missing metric lines")
	}
}
