// SPDX-License-Identifier: AGPL-3.0-only

// Package wakelatency exposes the JSON envelope written by the
// wake-reconcile sweep latency gate. The bench itself
// lives in wakelatency_test.go behind the `eval` build tag; this file is
// build-tag-free so the result type stays importable from documentation /
// reporting tooling.
package wakelatency

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Result is the on-disk envelope written by the wake-latency bench.
// TypicalP95MS is the p95 sweep wall time over N InjectWake iterations on
// the "typical repo" tree (TypicalFiles files); the NFR gate is < 500ms.
// LargeWorstMS is the single worst-case sweep on the >50k tree
// (LargeFiles files); the NFR gate is < 5000ms. ExitGateMet is the AND of
// both gates.
type Result struct {
	TypicalFiles int       `json:"typical_files"`
	LargeFiles   int       `json:"large_files"`
	Iterations   int       `json:"iterations"`
	TypicalP95MS float64   `json:"typical_p95_ms"`
	TypicalMinMS float64   `json:"typical_min_ms"`
	TypicalMaxMS float64   `json:"typical_max_ms"`
	LargeWorstMS float64   `json:"large_worst_ms"`
	GateP95MS    float64   `json:"gate_p95_ms"`
	GateLargeMS  float64   `json:"gate_large_ms"`
	ExitGateMet  bool      `json:"exit_gate_met"`
	Backend      string    `json:"backend"`
	Timestamp    time.Time `json:"timestamp"`
}

// WriteJSON marshals r to path with a trailing newline.
func WriteJSON(path string, r Result) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
