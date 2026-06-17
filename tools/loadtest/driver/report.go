//go:build loadtest

package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// GateStatus represents the verdict for a single exit gate.
type GateStatus int

const (
	StatusPending GateStatus = iota
	StatusPass
	StatusFail
)

// String returns the display string for a GateStatus.
func (s GateStatus) String() string {
	switch s {
	case StatusPass:
		return "PASS"
	case StatusFail:
		return "FAIL"
	default:
		return "PENDING"
	}
}

// GateResult holds the result for one M1 exit gate.
type GateResult struct {
	ID       int
	Name     string
	Budget   string
	Measured string
	Status   GateStatus
	Note     string
}

// renderReport writes the markdown report table to w.
func renderReport(gates []GateResult, w io.Writer) error {
	_, err := fmt.Fprintf(w, "# M1 Exit-Gate Report\n\nGenerated: %s\n\n", time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return err
	}

	header := "| Gate | Budget | Measured | Verdict |\n|------|--------|----------|---------|"
	if _, err = fmt.Fprintln(w, header); err != nil {
		return err
	}

	for _, g := range gates {
		measured := g.Measured
		if measured == "" {
			measured = "-"
		}
		note := ""
		if g.Note != "" {
			note = " (" + g.Note + ")"
		}
		line := fmt.Sprintf("| %d. %s | %s | %s | %s%s |",
			g.ID, g.Name, g.Budget, measured, g.Status.String(), note)
		if _, err = fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// WriteReport renders the gate results to a markdown file at path.
func WriteReport(gates []GateResult, path string) error {
	// Build content in memory first, then write atomically via os.WriteFile.
	var buf strings.Builder
	if err := renderReport(gates, &buf); err != nil {
		return err
	}
	return writeFile(path, []byte(buf.String()))
}

// exitCode returns the appropriate exit code for the gate set:
//
//	0 - all non-pending gates pass
//	1 - at least one gate fails
//	2 - no failures, but at least one gate is pending
func exitCode(gates []GateResult) int {
	hasPending := false
	for _, g := range gates {
		if g.Status == StatusFail {
			return 1
		}
		if g.Status == StatusPending {
			hasPending = true
		}
	}
	if hasPending {
		return 2
	}
	return 0
}
