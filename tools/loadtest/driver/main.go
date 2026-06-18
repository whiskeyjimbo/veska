//go:build loadtest

// Command loadtest reads M1 exit-gate RESULTS.md files and emits a consolidated
// REPORT.md under tools/loadtest/.
// Exit codes:
//
//	0 - all non-pending gates pass
//	1 - at least one gate fails
//	2 - no failures but at least one gate is pending
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	// Locate the tools/loadtest root relative to the binary's working directory.
	// When invoked via `make loadtest` the cwd is the module root.
	ltRoot := filepath.Join("tools", "loadtest")

	gates := buildGates(ltRoot)

	// Print to stdout
	if err := renderReport(gates, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "render error: %v\n", err)
		os.Exit(1)
	}

	// Write REPORT.md
	reportPath := filepath.Join(ltRoot, "REPORT.md")
	if err := WriteReport(gates, reportPath); err != nil {
		fmt.Fprintf(os.Stderr, "write report error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", reportPath)

	os.Exit(exitCode(gates))
}

// buildGates collects all gate results from the RESULTS.md files.
func buildGates(ltRoot string) []GateResult {
	gates := make([]GateResult, 0, 8)

	// Gate 1: Cold-scan 100k LOC < 60s
	gates = append(gates, gate1ColdScan(ltRoot))

	// Gate 2: find_symbol warm p95 < 50ms
	gates = append(gates, gate2MCPLatency(ltRoot))

	// Gate 3: Post-commit hook return p95 < 100ms
	gates = append(gates, gate3HookBench(ltRoot))

	// Gate 4: Daemon RSS ≤ 2 GiB
	gates = append(gates, gate4RSS(ltRoot))

	// Gate 5: Promotion 50k-symbol refactor < 5s p95
	gates = append(gates, gate5Promotion(ltRoot))

	// Gate 6: semantic_search p95 < 100ms at 50k vectors
	gates = append(gates, gate6VectorBench(ltRoot))

	// Gates 7 & 8: race tests + lint - read from ci-gates/RESULTS.md if present
	g7, g8 := gatesCIGates(ltRoot)
	gates = append(gates, g7, g8)

	return gates
}

func gate1ColdScan(ltRoot string) GateResult {
	g := GateResult{
		ID:     1,
		Name:   "Cold-scan 100k LOC",
		Budget: "<60s",
	}
	content, err := readFile(filepath.Join(ltRoot, "cold-scan", "RESULTS.md"))
	if err != nil {
		g.Status = StatusPending
		g.Note = "RESULTS.md not found"
		return g
	}
	val, pass, err := parseColdScan(content)
	if err != nil {
		g.Status = StatusFail
		g.Note = err.Error()
		return g
	}
	g.Measured = val
	if pass {
		g.Status = StatusPass
	} else {
		g.Status = StatusFail
	}
	return g
}

func gate2MCPLatency(ltRoot string) GateResult {
	g := GateResult{
		ID:     2,
		Name:   "find_symbol warm p95",
		Budget: "<50ms",
	}
	content, err := readFile(filepath.Join(ltRoot, "mcp-latency", "RESULTS.md"))
	if err != nil {
		g.Status = StatusPending
		g.Note = "RESULTS.md not found"
		return g
	}
	ms, ok := parseMCPLatencyBench(content)
	if !ok {
		g.Status = StatusFail
		g.Note = "parseMCPLatencyBench: metric line not found"
		return g
	}
	g.Measured = fmt.Sprintf("%.3fms", ms)
	if ms < 50.0 {
		g.Status = StatusPass
	} else {
		g.Status = StatusFail
	}
	return g
}

func gate3HookBench(ltRoot string) GateResult {
	g := GateResult{
		ID:     3,
		Name:   "Post-commit hook p95",
		Budget: "<100ms",
	}
	content, err := readFile(filepath.Join(ltRoot, "hook-bench", "RESULTS.md"))
	if err != nil {
		g.Status = StatusPending
		g.Note = "RESULTS.md not found"
		return g
	}
	val, pass, err := parseHookBench(content)
	if err != nil {
		g.Status = StatusFail
		g.Note = err.Error()
		return g
	}
	g.Measured = val
	if pass {
		g.Status = StatusPass
	} else {
		g.Status = StatusFail
	}
	return g
}

func gate4RSS(ltRoot string) GateResult {
	g := GateResult{
		ID:     4,
		Name:   "Daemon RSS",
		Budget: "≤2GiB",
	}
	content, err := readFile(filepath.Join(ltRoot, "multi-branch", "RESULTS.md"))
	if err != nil {
		g.Status = StatusPending
		g.Note = "RESULTS.md not found"
		return g
	}
	rssBytes, _, ok := parseMultiBranchBench(content)
	if !ok {
		g.Status = StatusFail
		g.Note = "parseMultiBranchBench: metric line not found"
		return g
	}
	const gib2 = 2 * 1024 * 1024 * 1024
	rssMiB := float64(rssBytes) / 1024 / 1024
	if rssMiB >= 1024 {
		g.Measured = fmt.Sprintf("%.2fGiB", rssMiB/1024)
	} else {
		g.Measured = fmt.Sprintf("%dmb", int64(rssMiB))
	}
	if rssBytes == 0 {
		// Platform returned 0 (non-Linux) - treat as pending.
		g.Status = StatusPending
		g.Note = "RSS measurement unavailable on this platform"
		return g
	}
	if rssBytes <= gib2 {
		g.Status = StatusPass
	} else {
		g.Status = StatusFail
	}
	return g
}

func gate5Promotion(ltRoot string) GateResult {
	g := GateResult{
		ID:     5,
		Name:   "Promotion 50k-symbol refactor",
		Budget: "<5s p95",
	}
	content, err := readFile(filepath.Join(ltRoot, "multi-branch", "RESULTS.md"))
	if err != nil {
		g.Status = StatusPending
		g.Note = "RESULTS.md not found"
		return g
	}
	_, p95Secs, ok := parseMultiBranchBench(content)
	if !ok {
		g.Status = StatusFail
		g.Note = "parseMultiBranchBench: metric line not found"
		return g
	}
	g.Measured = fmt.Sprintf("%.2fs", p95Secs)
	if p95Secs < 5.0 {
		g.Status = StatusPass
	} else {
		g.Status = StatusFail
	}
	return g
}

func gate6VectorBench(ltRoot string) GateResult {
	g := GateResult{
		ID:     6,
		Name:   "semantic_search p95 @50k vectors",
		Budget: "<100ms",
	}
	content, err := readFile(filepath.Join(ltRoot, "spikes", "hnsw", "cmd", "vector-bench", "RESULTS.md"))
	if err != nil {
		g.Status = StatusPending
		g.Note = "RESULTS.md not found"
		return g
	}
	val, pass, err := parseVectorBench(content)
	if err != nil {
		g.Status = StatusFail
		g.Note = err.Error()
		return g
	}
	g.Measured = val
	if pass {
		g.Status = StatusPass
	} else {
		g.Status = StatusFail
	}
	return g
}

func gatesCIGates(ltRoot string) (GateResult, GateResult) {
	g7 := GateResult{
		ID:     7,
		Name:   "All tests pass -race",
		Budget: "all pass",
	}
	g8 := GateResult{
		ID:     8,
		Name:   "golangci-lint + layercheck clean",
		Budget: "clean",
	}

	content, err := readFile(filepath.Join(ltRoot, "ci-gates", "RESULTS.md"))
	if err != nil {
		// ci-gates/RESULTS.md absent - mark pending
		g7.Status = StatusPending
		g7.Measured = "see CI"
		g8.Status = StatusPending
		g8.Measured = "see CI"
		return g7, g8
	}

	racePassed, lintPassed, err := parseCIGates(content)
	if err != nil {
		g7.Status = StatusPending
		g7.Measured = "see CI"
		g8.Status = StatusPending
		g8.Measured = "see CI"
		return g7, g8
	}

	g7.Measured = "see CI"
	if racePassed {
		g7.Status = StatusPass
	} else {
		g7.Status = StatusFail
	}

	g8.Measured = "see CI"
	if lintPassed {
		g8.Status = StatusPass
	} else {
		g8.Status = StatusFail
	}

	return g7, g8
}

// readFile reads the entire content of a file as a string.
func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// writeFile writes data to path, creating parent directories as needed.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
