package autolink

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Result is the JSON envelope written by the auto-link FP eval
// harness. Field names match the m3.04.4 DoD so downstream M3-close
// tooling can read it without translation.
type Result struct {
	Population          int       `json:"population"`
	Clusters            int       `json:"clusters"`
	NodesPerCluster     int       `json:"nodes_per_cluster"`
	CandidatesPerSource int       `json:"candidates_per_source"`
	Threshold           float64   `json:"threshold"`
	FPRate              float64   `json:"fp_rate"`
	FP                  int       `json:"fp"`
	TP                  int       `json:"tp"`
	TotalCandidates     int       `json:"total_candidates"`
	Embedder            string    `json:"embedder"`
	Backend             string    `json:"backend"`
	Timestamp           time.Time `json:"timestamp"`
}

// WriteJSON writes r to path as pretty-printed JSON.
func WriteJSON(path string, r Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("autolink: WriteJSON mkdir: %w", err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("autolink: WriteJSON marshal: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("autolink: WriteJSON write: %w", err)
	}
	return nil
}
