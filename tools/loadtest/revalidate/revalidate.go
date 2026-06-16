// Package revalidate exposes the JSON envelope written by the m3.05.4
// revalidation timing bench. The bench itself lives in revalidate_test.go
// behind the `eval` build tag; this file is build-tag-free so the result
// type stays importable from documentation / reporting tooling.
package revalidate

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Result is the on-disk envelope written by the revalidation bench.
// elapsed_ms is the wall-clock time spent inside the per-file Handle loop
// only (fixture seeding is excluded). exit_gate_met == (elapsed_ms < 60000).
type Result struct {
	Nodes         int       `json:"nodes"`
	Files         int       `json:"files"`
	Edges         int       `json:"edges"`
	FindingsTotal int       `json:"findings_total"`
	FindingsStale int       `json:"findings_stale"`
	Refreshed     int       `json:"refreshed"`
	Closed        int       `json:"closed"`
	ElapsedMS     float64   `json:"elapsed_ms"`
	P95HandleMS   float64   `json:"p95_handle_ms"`
	ExitGateMet   bool      `json:"exit_gate_met"`
	Backend       string    `json:"backend"`
	Timestamp     time.Time `json:"timestamp"`
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
