// SPDX-License-Identifier: AGPL-3.0-only

//go:build loadtest

package main

import (
	"strings"
	"testing"
)

func TestWriteReport_ContainsAllGates(t *testing.T) {
	gates := []GateResult{
		{ID: 1, Name: "Cold-scan 100k LOC", Budget: "<60s", Measured: "1.616s", Status: StatusPass},
		{ID: 2, Name: "find_symbol warm p95", Budget: "<50ms", Measured: "", Status: StatusPending},
		{ID: 3, Name: "Post-commit hook p95", Budget: "<100ms", Measured: "0.116ms", Status: StatusPass},
		{ID: 4, Name: "Daemon RSS", Budget: "≤2GiB", Measured: "", Status: StatusPending},
		{ID: 5, Name: "Promotion 50k-symbol refactor", Budget: "<5s p95", Measured: "", Status: StatusPending},
		{ID: 6, Name: "semantic_search p95 @50k vectors", Budget: "<100ms", Measured: "1.90ms", Status: StatusPass},
		{ID: 7, Name: "All tests pass -race", Budget: "all pass", Measured: "see CI", Status: StatusPending},
		{ID: 8, Name: "golangci-lint + layercheck clean", Budget: "clean", Measured: "see CI", Status: StatusPending},
	}

	var buf strings.Builder
	if err := renderReport(gates, &buf); err != nil {
		t.Fatalf("renderReport error: %v", err)
	}
	out := buf.String()

	// Check header
	if !strings.Contains(out, "# M1 Exit-Gate Report") {
		t.Error("missing report header")
	}
	// Check table header
	if !strings.Contains(out, "| Gate |") {
		t.Error("missing table header")
	}
	// Check each gate name appears
	for _, g := range gates {
		if !strings.Contains(out, g.Name) {
			t.Errorf("missing gate %q in report", g.Name)
		}
	}
	// PASS gates show measured value
	if !strings.Contains(out, "1.616s") {
		t.Error("missing measured value for cold-scan")
	}
	// PENDING gates show PENDING
	if !strings.Contains(out, "PENDING") {
		t.Error("missing PENDING in report")
	}
	// PASS gates show PASS
	if !strings.Contains(out, "PASS") {
		t.Error("missing PASS in report")
	}
}

func TestExitCode(t *testing.T) {
	allPass := []GateResult{
		{Status: StatusPass},
		{Status: StatusPass},
	}
	if code := exitCode(allPass); code != 0 {
		t.Errorf("all pass: want 0, got %d", code)
	}

	withPending := []GateResult{
		{Status: StatusPass},
		{Status: StatusPending},
	}
	if code := exitCode(withPending); code != 2 {
		t.Errorf("with pending: want 2, got %d", code)
	}

	withFail := []GateResult{
		{Status: StatusPass},
		{Status: StatusFail},
		{Status: StatusPending},
	}
	if code := exitCode(withFail); code != 1 {
		t.Errorf("with fail: want 1, got %d", code)
	}

	onlyFail := []GateResult{
		{Status: StatusFail},
	}
	if code := exitCode(onlyFail); code != 1 {
		t.Errorf("only fail: want 1, got %d", code)
	}
}

func TestGateStatusString(t *testing.T) {
	cases := []struct {
		s    GateStatus
		want string
	}{
		{StatusPass, "PASS"},
		{StatusFail, "FAIL"},
		{StatusPending, "PENDING"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("GateStatus(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}
