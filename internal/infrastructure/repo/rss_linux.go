//go:build linux

package repo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CurrentRSS returns the current process RSS in bytes by reading /proc/self/status.
func CurrentRSS() (int64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("read /proc/self/status: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		after, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		// Value is in kB, e.g. "VmRSS: 12345 kB"
		fields := strings.Fields(after)
		if len(fields) == 0 {
			return 0, fmt.Errorf("unexpected VmRSS format: %q", line)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse VmRSS: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("VmRSS not found in /proc/self/status")
}
