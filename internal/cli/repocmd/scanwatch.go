package repocmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// logTailBytes bounds how much of the daemon log each tail* helper inspects,
// keeping the wait loop snappy on a large log file.
const logTailBytes = 64 * 1024

// scanLogTail invokes onLine for each line in the last logTailBytes of
// logPath. Best-effort: a missing/unreadable log is a silent no-op so the
// CLI degrades to whatever default the caller computed.
func scanLogTail(logPath string, onLine func(line string)) {
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return
	}
	offset := int64(0)
	if info.Size() > logTailBytes {
		offset = info.Size() - logTailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
}

// tailScanFailureReason scans the tail of daemon.log for the most recent
// ERROR/WARN line referencing repoID and returns a short reason string
// suitable for inline display. Best-effort: returns "" when no matching
// line is found, the log is unreadable, or the JSONL line cannot be parsed.
func tailScanFailureReason(logPath, repoID string) string {
	var lastReason string
	scanLogTail(logPath, func(line string) {
		if !strings.Contains(line, repoID) {
			return
		}
		if !strings.Contains(line, `"level":"ERROR"`) && !strings.Contains(line, `"level":"WARN"`) {
			return
		}
		var rec struct {
			Msg string `json:"msg"`
			Err string `json:"err"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return
		}
		switch {
		case rec.Err != "" && rec.Msg != "":
			lastReason = rec.Msg + ": " + rec.Err
		case rec.Err != "":
			lastReason = rec.Err
		case rec.Msg != "":
			lastReason = rec.Msg
		}
	})
	return lastReason
}

// tailScanCompleteFiles scans the daemon log for the most-recent
// "cold scan: complete" entry for repoID and returns its files_saved
// count. Used as a last-resort source when the scan finished too fast
// for the CLI's poll loop to observe a non-zero FilesSeen (solov2-a17i).
func tailScanCompleteFiles(logPath, repoID string) int {
	var lastFiles int
	scanLogTail(logPath, func(line string) {
		if !strings.Contains(line, repoID) || !strings.Contains(line, "cold scan: complete") {
			return
		}
		var rec struct {
			FilesSaved int `json:"files_saved"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err == nil && rec.FilesSaved > 0 {
			lastFiles = rec.FilesSaved
		}
	})
	return lastFiles
}

// tailVulnScanResult scans the daemon log for the most-recent
// "vuln-scan: scanned" entry for repoID and returns (deps, findings,
// found). found=false when the scanner didn't run (NullVulnSource —
// [vuln_source] not configured) so callers can omit the summary line
// rather than print a misleading "0 findings". solov2-izh6.3.
func tailVulnScanResult(logPath, repoID string) (deps, findings int, found bool) {
	scanLogTail(logPath, func(line string) {
		if !strings.Contains(line, repoID) || !strings.Contains(line, "vuln-scan: scanned") {
			return
		}
		var rec struct {
			Deps     int `json:"deps"`
			Findings int `json:"findings"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			deps, findings, found = rec.Deps, rec.Findings, true
		}
	})
	return deps, findings, found
}

// scanWaitTuning groups the poll-loop timing constants for WaitForScanComplete.
// See the field comments for the per-constant rationale.
const (
	// heartbeatEvery: solov2-en47 — when the scanner sits on a slow file the
	// phase + files_seen don't change, so the original loop printed nothing
	// for tens of seconds. Heartbeat with elapsed-since-last-update so the
	// user can see we're still working.
	heartbeatEvery = 10 * time.Second
	// pollInterval: solov2-a17i — poll at 100ms (was 500ms). Small repos walk
	// + promote in well under 500ms; the old cadence usually missed every
	// intermediate files_seen update.
	pollInterval = 100 * time.Millisecond
	// startupGrace bounds how long --wait keeps polling while the scan has
	// not yet appeared in scans_in_flight, so the very first poll firing
	// before the scheduler enqueues the scan does not surface a misleading
	// "scan no longer in flight" failure (solov2-beda).
	startupGrace = 5 * time.Second
)

// waitState tracks the per-iteration bookkeeping of the WaitForScanComplete
// poll loop so the loop body stays small enough for the size gate.
type waitState struct {
	start       time.Time
	lastPhase   string
	lastFiles   int
	maxFiles    int
	sawInFlight bool
	lastEvent   time.Time
}

// WaitForScanComplete polls eng_get_status until the named repo's scan
// has left scans_in_flight, printing one progress line per phase change
// or files-seen jump so the user has a continuous signal instead of a
// silent background scan.
func WaitForScanComplete(ctx context.Context, w io.Writer, repoID string) error {
	st := &waitState{start: time.Now(), lastEvent: time.Now()}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		progress := FetchScanProgress(ctx)
		row, inFlight := progress[repoID]
		if !inFlight {
			done, err := st.handleNotInFlight(ctx, w, repoID)
			if done {
				return err
			}
			time.Sleep(pollInterval)
			continue
		}
		st.sawInFlight = true
		st.reportProgress(w, row)
		time.Sleep(pollInterval)
	}
}

// handleNotInFlight resolves the "repo no longer in scans_in_flight" case.
// done=false means stay in the loop (startup grace not yet elapsed); done=true
// means the wait is over and err carries the terminal result.
func (st *waitState) handleNotInFlight(ctx context.Context, w io.Writer, repoID string) (done bool, err error) {
	if st.reportCompleteIfPromoted(ctx, w, repoID) {
		return true, nil
	}
	// Not promoted and no in-flight entry. Either the scan never started
	// yet (stay in the loop until startupGrace elapses so we don't surface
	// a false-negative on the user's very first repo — solov2-beda) or a
	// previously in-flight scan left the set without a last_promoted_sha
	// (a real failure; surface the daemon log's cause).
	logPath := daemonLogPath()
	if !st.sawInFlight && time.Since(st.start) < startupGrace {
		return false, nil
	}
	if reason := tailScanFailureReason(logPath, repoID); reason != "" {
		fmt.Fprintf(w, "  ✗ cold scan failed: %s\n", reason)
		fmt.Fprintf(w, "    full context: tail %s\n", logPath)
		return true, fmt.Errorf("cold scan failed")
	}
	if !st.sawInFlight {
		fmt.Fprintf(w, "  ✗ scan never started after %.0fs — daemon may be wedged; tail %s\n", time.Since(st.start).Seconds(), logPath)
		return true, fmt.Errorf("cold scan did not start")
	}
	fmt.Fprintf(w, "  scan no longer in flight, repo not yet promoted — tail %s for the cause\n", logPath)
	return true, nil
}

// reportCompleteIfPromoted prints the completion summary (file count +
// vuln-scan result) when the repo now has a last_promoted_sha. Returns true
// when the scan is confirmed complete.
func (st *waitState) reportCompleteIfPromoted(ctx context.Context, w io.Writer, repoID string) bool {
	var lr listResult
	if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
		return false
	}
	for _, r := range lr.Repos {
		if r.RepoID != repoID || r.LastPromotedSHA == "" {
			continue
		}
		logPath := daemonLogPath()
		// solov2-a17i: scan may have finished before our first poll caught a
		// non-zero FilesSeen. Fall back to the daemon log's final files_saved.
		files := st.maxFiles
		if files == 0 {
			files = tailScanCompleteFiles(logPath, repoID)
		}
		printColdScanComplete(w, files, time.Since(st.start))
		// solov2-izh6.3: surface vuln-scan result so a fresh-init user can see
		// the OSV scanner did run. Silent when [vuln_source] is off.
		if deps, findings, ok := tailVulnScanResult(logPath, repoID); ok {
			fmt.Fprintf(w, "  ✓ vuln-scan: %d finding(s) across %d dep(s)\n", findings, deps)
		}
		return true
	}
	return false
}

// printColdScanComplete prints the "✓ cold scan complete" line, with the file
// count when one is known.
func printColdScanComplete(w io.Writer, files int, elapsed time.Duration) {
	if files <= 0 {
		fmt.Fprintf(w, "  ✓ cold scan complete (%.1fs)\n", elapsed.Seconds())
		return
	}
	plural := "s"
	if files == 1 {
		plural = ""
	}
	fmt.Fprintf(w, "  ✓ cold scan complete: %d file%s (%.1fs)\n", files, plural, elapsed.Seconds())
}

// reportProgress prints a progress line on a phase change / files-seen jump,
// or a heartbeat line when the scan has stalled past heartbeatEvery.
func (st *waitState) reportProgress(w io.Writer, row ScanProgressRow) {
	if row.FilesSeen > st.maxFiles {
		st.maxFiles = row.FilesSeen
	}
	// solov2-a17i: suppress the "0 files (0.0s)" first tick — it just reflects
	// the race between scan start and the first poll and reads as broken. We
	// still report when files_seen first crosses 0 or when phase changes after
	// we've seen files.
	if row.Phase != st.lastPhase || row.FilesSeen != st.lastFiles {
		meaningful := row.FilesSeen > 0 || (st.lastPhase != "" && row.Phase != st.lastPhase)
		if meaningful {
			fmt.Fprintf(w, "  %s → %d files (%.1fs)\n", phaseOrRunning(row.Phase), row.FilesSeen, time.Since(st.start).Seconds())
			st.lastEvent = time.Now()
		}
		st.lastPhase = row.Phase
		st.lastFiles = row.FilesSeen
		return
	}
	if time.Since(st.lastEvent) >= heartbeatEvery {
		fmt.Fprintf(w, "  %s → %d files (%.1fs, stalled %.0fs — check ~/.veska/logs/daemon.log)\n",
			phaseOrRunning(row.Phase), row.FilesSeen, time.Since(st.start).Seconds(), time.Since(st.lastEvent).Seconds())
		st.lastEvent = time.Now()
	}
}

func phaseOrRunning(phase string) string {
	if phase == "" {
		return "running"
	}
	return phase
}
