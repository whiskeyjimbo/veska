// Package savings records per-search token-savings telemetry to a
// JSONL file (~/.veska/savings.jsonl by default) and reports rollups
// for the `veska doctor savings` subcommand.
// The premise: when search returns inline snippets, the agent skips a
// follow-up Read of the matched files. The savings ratio is
//
//	1 - sum(snippet_chars) / sum(unique_file_chars)
//
// which is the marketing number Semble's "semble savings" chart
// surfaces. It is cheap to compute, has no fan-out beyond a single
// O_APPEND write per search, and is local-only — no network egress.
// The Recorder is intentionally optional: a nil *Recorder is the
// "disabled" state and silently no-ops, so callers don't need to
// guard every call site with an explicit feature check.
package savings

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is a single recorded search call. JSON tags are stable across
// versions — the aggregator must keep being able to read entries
// written by older daemons after an upgrade.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	// RepoID tags the entry with the repo its results came from so
	// Aggregate can bucket per repo. omitempty keeps
	// back-compat: entries written by pre-0ql0 daemons have no repo_id
	// and aggregate into the "" bucket.
	RepoID       string `json:"repo_id,omitempty"`
	Query        string `json:"query"`
	Results      int    `json:"n_results"`
	FileChars    int64  `json:"file_chars"`
	SnippetChars int64  `json:"snippet_chars"`
}

// ResultFile is the minimal projection EntryFor needs from a search
// result: a file path to stat and a snippet length to sum. Keeping
// this struct narrow lets infrastructure callers (MCP, CLI) build the
// input without leaking search-package types into this package.
type ResultFile struct {
	FilePath   string
	SnippetLen int
}

// Recorder appends Entry records to a JSONL file. A single recorder
// is safe for concurrent Record calls — writes are serialised through
// a mutex so partial lines never interleave. fsync is intentionally
// omitted (acceptance criterion 3): a power-loss-induced loss of the
// last few savings entries is acceptable; the hot-path overhead of
// fsync would not be.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
}

// NewRecorder opens (or creates) path in append-only mode. The parent
// directory must already exist — the caller's data-dir initialisation
// is responsible for it.
func NewRecorder(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("savings: open %s: %w", path, err)
	}
	return &Recorder{f: f}, nil
}

// Record appends e as a single JSONL line. A nil receiver is the
// documented "disabled" state — returns nil and does nothing.
func (r *Recorder) Record(e Entry) error {
	if r == nil {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("savings: marshal entry: %w", err)
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.f.Write(line); err != nil {
		return fmt.Errorf("savings: write: %w", err)
	}
	return nil
}

// Close releases the underlying file. A nil receiver is a no-op.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// EntryFor builds an Entry from a repo_id, query and the result-file
// projection. File chars are summed over UNIQUE file paths (so the same
// file matching three times still counts its size once); snippet chars
// are summed across all results. Files that no longer exist on disk
// silently contribute 0 to FileChars — a delete-then-search race must
// not crash the recorder. repoID tags the entry so a fanout search that
// spans multiple repos records one Entry per repo.
// root is the repo's absolute working-tree root. Since
// nodes.file_path (and thus ResultFile.FilePath) is repo-relative, so the
// on-disk stat must rejoin root. An empty root means "unknown" — the stat
// falls back to the path as-is, contributing 0 for a relative path rather
// than crashing.
func EntryFor(repoID, root, query string, results []ResultFile, now time.Time) Entry {
	var snippet int64
	seen := make(map[string]struct{}, len(results))
	var fileBytes int64
	for _, r := range results {
		snippet += int64(r.SnippetLen)
		if r.FilePath == "" {
			continue
		}
		if _, ok := seen[r.FilePath]; ok {
			continue
		}
		seen[r.FilePath] = struct{}{}
		abs := r.FilePath
		if root != "" {
			abs = filepath.Join(root, r.FilePath)
		}
		if info, err := os.Stat(abs); err == nil {
			fileBytes += info.Size()
		}
	}
	return Entry{
		Timestamp:    now,
		RepoID:       repoID,
		Query:        query,
		Results:      len(results),
		FileChars:    fileBytes,
		SnippetChars: snippet,
	}
}

// Period is the rolled-up savings over a window: how many search
// calls landed in it, how much file content would have been pulled,
// how much snippet content actually was. SavingsRatio derives from
// these two sums.
type Period struct {
	Label        string
	Since        time.Time
	Calls        int
	FileChars    int64
	SnippetChars int64
}

// SavingsRatio is 1 - snippet/file. An empty period (FileChars=0)
// returns 0 rather than NaN so the doctor chart can render uniformly.
func (p Period) SavingsRatio() float64 {
	if p.FileChars == 0 {
		return 0
	}
	return 1 - float64(p.SnippetChars)/float64(p.FileChars)
}

// Report is the three-window rollup the doctor subcommand renders.
type Report struct {
	Today   Period
	Last7d  Period
	AllTime Period
}

// Aggregate streams path and produces a Report rolled up against now,
// pooling every repo into one report. Today is the local-day window
// containing now; Last7d is the trailing 7 days (today included);
// AllTime spans every entry in the file. A missing file is not an
// error — the caller (a fresh install with no searches yet) just sees a
// zero report.
func Aggregate(path string, now time.Time) (Report, error) {
	rep := newReport(now)
	err := scanEntries(path, rep.addEntry)
	return rep, err
}

// AggregateByRepo is Aggregate partitioned by Entry.RepoID: it returns
// one Report per repo seen in the file. Entries written by
// pre-0ql0 daemons carry no repo_id and bucket under the "" key. Each
// per-repo Report uses the same period-bucketing as Aggregate, so the
// combined Aggregate equals the sum of these by construction. A missing
// file yields an empty map and no error.
func AggregateByRepo(path string, now time.Time) (map[string]Report, error) {
	byRepo := map[string]*Report{}
	err := scanEntries(path, func(e Entry) {
		rep, ok := byRepo[e.RepoID]
		if !ok {
			r := newReport(now)
			rep = &r
			byRepo[e.RepoID] = rep
		}
		rep.addEntry(e)
	})
	out := make(map[string]Report, len(byRepo))
	for id, rep := range byRepo {
		out[id] = *rep
	}
	return out, err
}

// newReport returns a zeroed Report with the period windows anchored on
// now: Today is the local day containing now, Last7d the trailing 7 days
// (today included), AllTime unbounded.
func newReport(now time.Time) Report {
	return Report{
		Today:   Period{Label: "today", Since: startOfDay(now)},
		Last7d:  Period{Label: "last_7d", Since: startOfDay(now).AddDate(0, 0, -6)},
		AllTime: Period{Label: "all_time"},
	}
}

// addEntry folds e into whichever of the report's windows contain it.
func (rep *Report) addEntry(e Entry) {
	rep.AllTime.add(e)
	if !e.Timestamp.Before(rep.Last7d.Since) {
		rep.Last7d.add(e)
	}
	if !e.Timestamp.Before(rep.Today.Since) {
		rep.Today.add(e)
	}
}

// scanEntries streams the JSONL file at path, invoking fn for each
// well-formed Entry. A missing file is not an error (fresh install). One
// corrupt line — most likely a truncated last line from a crashed write
// is skipped rather than aborting the whole scan.
func scanEntries(path string, fn func(Entry)) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("savings: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Bump max line length: savings entries are tiny today, but a
	// future Query that's a long natural-language sentence could blow
	// the default 64KiB. 1 MiB is plenty.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		fn(e)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("savings: scan %s: %w", path, err)
	}
	return nil
}

func (p *Period) add(e Entry) {
	p.Calls++
	p.FileChars += e.FileChars
	p.SnippetChars += e.SnippetChars
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
