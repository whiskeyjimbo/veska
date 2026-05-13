//go:build loadtest

package main

import (
	"errors"
	"strings"
)

// parseColdScan reads a cold-scan RESULTS.md body and returns
// (measured value, pass bool, error).
// It looks for "| Total elapsed |" to extract the measured value,
// and "Gate: PASS" to determine verdict.
func parseColdScan(content string) (string, bool, error) {
	measured := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Total elapsed") && strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			// fields: ["", " Total elapsed ", " 1.616s ", " <60s ", ""]
			if len(fields) >= 3 {
				measured = strings.TrimSpace(fields[2])
			}
		}
	}
	if measured == "" {
		return "", false, errors.New("parseColdScan: metric line not found")
	}
	pass := strings.Contains(content, "Gate: PASS")
	return measured, pass, nil
}

// parseHookBench reads a hook-bench RESULTS.md body and returns
// (p95 value, pass bool, error).
func parseHookBench(content string) (string, bool, error) {
	measured := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "p95 latency") && strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			// fields: ["", " p95 latency ", " 0.116ms ", " ≤100ms ", ""]
			if len(fields) >= 3 {
				measured = strings.TrimSpace(fields[2])
			}
		}
	}
	if measured == "" {
		return "", false, errors.New("parseHookBench: p95 latency line not found")
	}
	pass := strings.Contains(content, "Gate: PASS")
	return measured, pass, nil
}

// parseVectorBench reads a vector-bench RESULTS.md body and returns
// (p95 at 50k, pass bool, error).
// It looks for the 50k table row to extract the P95 value and the ✓ pass indicator.
func parseVectorBench(content string) (string, bool, error) {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "| 50k") && strings.Contains(line, "|") {
			// "| 50k | 0.9870 (≥0.95) | 1.90 (≤100) | 0.95 | 100 | ✓ |"
			fields := strings.Split(line, "|")
			if len(fields) >= 4 {
				// fields[3] is the P95 cell: " 1.90 (≤100) "
				cell := strings.TrimSpace(fields[3])
				// Extract just the numeric part before the space
				p95 := cell
				if idx := strings.Index(cell, " "); idx > 0 {
					p95 = cell[:idx]
				}
				pass := strings.Contains(line, "✓")
				return p95 + "ms", pass, nil
			}
		}
	}
	return "", false, errors.New("parseVectorBench: 50k row not found")
}

// parseCIGates reads a ci-gates RESULTS.md body and returns
// (racePassed, lintPassed bool, error).
// It expects rows for "race tests" and "golangci-lint".
func parseCIGates(content string) (bool, bool, error) {
	raceFound := false
	lintFound := false
	racePassed := false
	lintPassed := false

	for _, line := range strings.Split(content, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "race") && strings.Contains(line, "|") {
			raceFound = true
			racePassed = strings.Contains(line, "PASS")
		}
		if (strings.Contains(lower, "golangci-lint") || strings.Contains(lower, "lint")) && strings.Contains(line, "|") {
			lintFound = true
			lintPassed = strings.Contains(line, "PASS")
		}
	}

	if !raceFound || !lintFound {
		return false, false, errors.New("parseCIGates: required gate rows not found")
	}
	return racePassed, lintPassed, nil
}
