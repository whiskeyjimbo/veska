// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ResetReport describes what ResetCrashLoop cleared.
type ResetReport struct {
	BrokenMarkerCleared bool `json:"broken_marker_cleared"`
	CrashCountCleared   bool `json:"crash_count_cleared"`
	CrashCountWas       int  `json:"crash_count_was"`
}

// ResetCrashLoop removes the broken-marker file (<veskaHome>/broken) and the
// crash-count file (<veskaHome>/crash_count) if they are present. It returns
// a ResetReport describing what was cleared and what the crash count was before
// deletion. If neither file exists the call succeeds and both cleared fields
// are false.
func ResetCrashLoop(veskaHome string) (ResetReport, error) {
	var report ResetReport

	markerPath := filepath.Join(veskaHome, "broken")
	countPath := filepath.Join(veskaHome, "crash_count")

	// Read crash count before removing anything.
	if raw, err := os.ReadFile(countPath); err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		report.CrashCountWas = n
	}

	// Remove broken marker.
	if err := os.Remove(markerPath); err == nil {
		report.BrokenMarkerCleared = true
	} else if !os.IsNotExist(err) {
		return ResetReport{}, err
	}

	// Remove crash count.
	if err := os.Remove(countPath); err == nil {
		report.CrashCountCleared = true
	} else if !os.IsNotExist(err) {
		return ResetReport{}, err
	}

	return report, nil
}
