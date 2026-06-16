// Package savings records per-search token-savings telemetry to a JSONL file
// (~/.veska/savings.jsonl by default) and reports rollups for the doctor subcommand.
// Telemetry is cheap to compute and local-only, using O_APPEND write per search.
// A nil *Recorder represents the disabled state and silently no-ops, allowing
// callers to invoke Record without explicit feature checks.
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

// Entry represents a single search call. JSON tags must remain stable
// across versions so that the aggregator can read older entries.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	// RepoID identifies the repository. Omitempty preserves compatibility
	// with older telemetry entries that omit the repository ID.
	RepoID       string `json:"repo_id,omitempty"`
	Query        string `json:"query"`
	Results      int    `json:"n_results"`
	FileChars    int64  `json:"file_chars"`
	SnippetChars int64  `json:"snippet_chars"`
}

// ResultFile is a narrow search result projection that prevents leaking
// search-package types into telemetry.
type ResultFile struct {
	FilePath   string
	SnippetLen int
}

// Recorder appends entries concurrently. Fsync is omitted to avoid hot-path
// performance overhead, accepting the risk of telemetry loss on power failure.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
}

// NewRecorder opens path. The parent directory must exist beforehand.
func NewRecorder(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("savings: open %s: %w", path, err)
	}
	return &Recorder{f: f}, nil
}

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

// EntryFor builds an Entry. Missing files contribute zero size to avoid
// crashing during search-then-delete races. Relative file paths are resolved
// against root, falling back to zero if root is empty.
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

type Period struct {
	Label        string
	Since        time.Time
	Calls        int
	FileChars    int64
	SnippetChars int64
}

// SavingsRatio returns 0 when FileChars is 0 to avoid NaN propagation.
func (p Period) SavingsRatio() float64 {
	if p.FileChars == 0 {
		return 0
	}
	return 1 - float64(p.SnippetChars)/float64(p.FileChars)
}

type Report struct {
	Today   Period
	Last7d  Period
	AllTime Period
}

// Aggregate builds a Report from path. A missing file is treated as a zero
// report to support fresh installations.
func Aggregate(path string, now time.Time) (Report, error) {
	rep := newReport(now)
	err := scanEntries(path, rep.addEntry)
	return rep, err
}

// AggregateByRepo aggregates telemetry partitioned by Entry.RepoID. Entries
// lacking a repository ID are bucketed under the empty string.
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

func newReport(now time.Time) Report {
	return Report{
		Today:   Period{Label: "today", Since: startOfDay(now)},
		Last7d:  Period{Label: "last_7d", Since: startOfDay(now).AddDate(0, 0, -6)},
		AllTime: Period{Label: "all_time"},
	}
}

func (rep *Report) addEntry(e Entry) {
	rep.AllTime.add(e)
	if !e.Timestamp.Before(rep.Last7d.Since) {
		rep.Last7d.add(e)
	}
	if !e.Timestamp.Before(rep.Today.Since) {
		rep.Today.add(e)
	}
}

// scanEntries processes the telemetry file. Corrupt or truncated lines are
// skipped to ensure a partial write does not invalidate the entire dataset.
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
	// Set 1MiB buffer limit to prevent failure on exceptionally long query strings.
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
