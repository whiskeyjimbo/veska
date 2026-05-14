// Package crashloop implements a crash-loop breaker for the engram daemon.
//
// It tracks restart frequency in two files under <engramHome>:
//   - crash_count        — integer restart counter for the current window
//   - crash_window_start — Unix timestamp (seconds) when the current window began
//   - broken             — presence of this file signals the breaker has tripped
//
// When five or more restarts occur within a 10-minute sliding window the
// breaker trips: it writes the broken marker and returns tripped=true from
// [Record].  The daemon should call [Check] at startup and exit with
// [ExitCode] (78) if the marker is present.  Run `engram doctor
// reset-crash-loop` to clear the marker and resume normal operation.
package crashloop

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExitCode is the UNIX sysexits EX_CONFIG exit status returned by the daemon
// when the crash-loop breaker has tripped.
const ExitCode = 78

const maxRestarts = 5
const windowDuration = 10 * time.Minute

// ErrBroken is returned by [Check] when the broken marker file is present.
var ErrBroken = errors.New("engram: crash-loop breaker tripped; run `engram doctor reset-crash-loop` to recover")

// Check returns [ErrBroken] if <engramHome>/broken exists; otherwise nil.
func Check(engramHome string) error {
	_, err := os.Stat(filepath.Join(engramHome, "broken"))
	if err == nil {
		return ErrBroken
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Record increments the restart counter for engramHome.
//
// It reads <engramHome>/crash_window_start to determine whether the current
// count falls within a 10-minute window.  If the window has expired (or never
// started) the counter and window-start are reset before incrementing.  If the
// counter reaches [maxRestarts] (5) the broken marker is written and
// tripped=true is returned.
//
// After the breaker has tripped every subsequent call to Record also returns
// tripped=true (the broken file already exists).
func Record(engramHome string) (tripped bool, err error) {
	// If already broken, remain tripped.
	if checkErr := Check(engramHome); checkErr != nil {
		if errors.Is(checkErr, ErrBroken) {
			return true, nil
		}
		return false, checkErr
	}

	now := time.Now()
	windowPath := filepath.Join(engramHome, "crash_window_start")
	countPath := filepath.Join(engramHome, "crash_count")

	// Determine whether we are inside the current window.
	inWindow := false
	if raw, readErr := os.ReadFile(windowPath); readErr == nil {
		ts, parseErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if parseErr == nil {
			windowStart := time.Unix(ts, 0)
			inWindow = now.Sub(windowStart) < windowDuration
		}
	}

	var count int
	if inWindow {
		if raw, readErr := os.ReadFile(countPath); readErr == nil {
			count, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
	} else {
		// Start a new window.
		if writeErr := os.WriteFile(windowPath, []byte(strconv.FormatInt(now.Unix(), 10)), 0o600); writeErr != nil {
			return false, writeErr
		}
		count = 0
	}

	count++

	if writeErr := os.WriteFile(countPath, []byte(strconv.Itoa(count)), 0o600); writeErr != nil {
		return false, writeErr
	}

	if count >= maxRestarts {
		brokenPath := filepath.Join(engramHome, "broken")
		if writeErr := os.WriteFile(brokenPath, []byte("crash-loop breaker tripped\n"), 0o600); writeErr != nil {
			return false, writeErr
		}
		return true, nil
	}

	return false, nil
}
