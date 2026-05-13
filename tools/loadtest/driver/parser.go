//go:build loadtest

package main

import (
	"errors"
	"math"
	"strconv"
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

// parseMCPLatencyBench parses find_symbol warm p95 from mcp-latency/RESULTS.md content.
// Returns latency in milliseconds and ok=true on success.
func parseMCPLatencyBench(content string) (ms float64, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "find_symbol warm p95") && strings.Contains(line, "|") {
			fields := strings.Split(line, "|")
			// fields: ["", " find_symbol warm p95 ", " 0.042ms ", " <50ms ", " PASS ", ""]
			if len(fields) < 3 {
				continue
			}
			raw := strings.TrimSpace(fields[2])
			raw = strings.TrimSuffix(raw, "ms")
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				continue
			}
			return f, true
		}
	}
	return 0, false
}

// parseMultiBranchBench parses daemon RSS (in bytes) and promotion p95 (in seconds)
// from multi-branch/RESULTS.md content.
//
// It expects table rows like:
//
//	| Daemon RSS | 342mb | ≤2GiB | PASS |
//	| Promotion 50k p95 | 0.82s | <5s | PASS |
func parseMultiBranchBench(content string) (rssBytes int64, promotionP95Secs float64, ok bool) {
	rssFound := false
	p95Found := false

	for _, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 3 {
			continue
		}
		label := strings.TrimSpace(fields[1])
		value := strings.TrimSpace(fields[2])

		switch {
		case strings.Contains(label, "Daemon RSS"):
			rssBytes = parseRSSValue(value)
			rssFound = true
		case strings.Contains(label, "Promotion 50k p95"):
			v := strings.TrimSuffix(value, "s")
			f, err := strconv.ParseFloat(v, 64)
			if err == nil {
				promotionP95Secs = f
				p95Found = true
			}
		}
	}

	if !rssFound || !p95Found {
		return 0, 0, false
	}
	return rssBytes, promotionP95Secs, true
}

// parseRSSValue converts an RSS string like "342mb" or "1.50GiB" to bytes.
func parseRSSValue(s string) int64 {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasSuffix(s, "GiB"):
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "GiB"), 64)
		if err != nil {
			return 0
		}
		return int64(math.Round(v * 1024 * 1024 * 1024))
	case strings.HasSuffix(s, "mb"):
		v, err := strconv.ParseInt(strings.TrimSuffix(s, "mb"), 10, 64)
		if err != nil {
			return 0
		}
		return v * 1024 * 1024
	default:
		return 0
	}
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
