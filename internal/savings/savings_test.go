package savings_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/savings"
)

// TestRecorder_AppendsJSONLPerCall covers acceptance criterion (1): one
// JSONL line per Record call, valid JSON, parseable back into an Entry.
func TestRecorder_AppendsJSONLPerCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.jsonl")

	r, err := savings.NewRecorder(path)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	for i, q := range []string{"alpha", "beta", "gamma"} {
		err := r.Record(savings.Entry{
			Timestamp:    time.Date(2026, 5, 20, 12, i, 0, 0, time.UTC),
			Query:        q,
			Results:      i + 1,
			FileChars:    1000 * int64(i+1),
			SnippetChars: 20 * int64(i+1),
		})
		if err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open jsonl: %v", err)
	}
	defer f.Close()

	var got []savings.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e savings.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line: %v (line=%q)", err, sc.Text())
		}
		got = append(got, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Query != "alpha" || got[1].Query != "beta" || got[2].Query != "gamma" {
		t.Errorf("queries out of order: %+v", got)
	}
	if got[2].FileChars != 3000 || got[2].SnippetChars != 60 {
		t.Errorf("third entry chars wrong: %+v", got[2])
	}
}

// TestRecorder_NilSafe: a nil recorder is the documented "disabled"
// state — Record/Close must not panic and must not produce a file.
func TestRecorder_NilSafe(t *testing.T) {
	var r *savings.Recorder
	if err := r.Record(savings.Entry{Query: "x"}); err != nil {
		t.Errorf("nil Record returned error: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("nil Close returned error: %v", err)
	}
}

// TestAggregate_BucketsByPeriod covers acceptance criterion (2): the
// reader splits entries into today / last-7-days / all-time buckets,
// each carrying summed file_chars + snippet_chars + call count. Today
// is contained in last-7-days; last-7-days is contained in all-time.
func TestAggregate_BucketsByPeriod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "savings.jsonl")

	// "now" anchors the period boundaries deterministically.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	entries := []savings.Entry{
		{Timestamp: now.Add(-1 * time.Hour), Query: "today_a", FileChars: 1000, SnippetChars: 10, Results: 1},
		{Timestamp: now.Add(-2 * time.Hour), Query: "today_b", FileChars: 2000, SnippetChars: 20, Results: 1},
		{Timestamp: now.Add(-3 * 24 * time.Hour), Query: "week_a", FileChars: 500, SnippetChars: 5, Results: 1},
		{Timestamp: now.Add(-30 * 24 * time.Hour), Query: "oldie", FileChars: 4000, SnippetChars: 100, Results: 1},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	_ = f.Close()

	rep, err := savings.Aggregate(path, now)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	if rep.Today.Calls != 2 || rep.Today.FileChars != 3000 || rep.Today.SnippetChars != 30 {
		t.Errorf("Today: %+v", rep.Today)
	}
	// Last 7 days includes today's 2 plus the -3d entry: 3 calls.
	if rep.Last7d.Calls != 3 || rep.Last7d.FileChars != 3500 || rep.Last7d.SnippetChars != 35 {
		t.Errorf("Last7d: %+v", rep.Last7d)
	}
	if rep.AllTime.Calls != 4 || rep.AllTime.FileChars != 7500 || rep.AllTime.SnippetChars != 135 {
		t.Errorf("AllTime: %+v", rep.AllTime)
	}
}

// TestAggregate_MissingFileIsEmpty: when the jsonl doesn't exist yet
// (no searches recorded), Aggregate must return a zeroed Report and
// no error — the doctor subcommand should print "no data" cleanly.
func TestAggregate_MissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	rep, err := savings.Aggregate(filepath.Join(dir, "nope.jsonl"), time.Now())
	if err != nil {
		t.Fatalf("Aggregate missing: %v", err)
	}
	if rep.AllTime.Calls != 0 {
		t.Errorf("expected zero calls on missing file, got %+v", rep.AllTime)
	}
}

// TestPeriod_Ratio: the "savings ratio" is 1 - snippet/file. Pinned so
// the doctor bar chart computes percentages correctly.
func TestPeriod_Ratio(t *testing.T) {
	p := savings.Period{FileChars: 10000, SnippetChars: 200}
	got := p.SavingsRatio()
	want := 0.98
	if abs := got - want; abs < -1e-9 || abs > 1e-9 {
		t.Errorf("SavingsRatio: got %v, want %v", got, want)
	}
	// Empty period reports 0.0, not NaN.
	empty := savings.Period{}
	if r := empty.SavingsRatio(); r != 0 {
		t.Errorf("empty ratio: got %v, want 0", r)
	}
}

// TestEntryFor_UniqueFileChars: when multiple results share a file,
// the file is counted only once toward FileChars. Snippet chars are
// summed across all results.
func TestEntryFor_UniqueFileChars(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.go")
	if err := os.WriteFile(fileA, []byte("aaaaa"), 0o644); err != nil { // 5 bytes
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("bbb"), 0o644); err != nil { // 3 bytes
		t.Fatal(err)
	}

	results := []savings.ResultFile{
		{FilePath: fileA, SnippetLen: 10},
		{FilePath: fileA, SnippetLen: 7}, // same file — count once
		{FilePath: fileB, SnippetLen: 4},
	}
	e := savings.EntryFor("test", results, time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC))
	if e.Results != 3 {
		t.Errorf("Results: got %d, want 3", e.Results)
	}
	if e.FileChars != 8 { // 5 + 3
		t.Errorf("FileChars: got %d, want 8", e.FileChars)
	}
	if e.SnippetChars != 21 { // 10 + 7 + 4
		t.Errorf("SnippetChars: got %d, want 21", e.SnippetChars)
	}
}

// TestEntryFor_MissingFileSilentlySkipped: a file that has since been
// deleted shouldn't break the recorder — it just contributes 0 to
// FileChars. Otherwise an in-flight delete would crash search.
func TestEntryFor_MissingFileSilentlySkipped(t *testing.T) {
	results := []savings.ResultFile{
		{FilePath: "/nonexistent/path/that/does/not/exist.go", SnippetLen: 5},
	}
	e := savings.EntryFor("q", results, time.Now())
	if e.FileChars != 0 {
		t.Errorf("missing file should contribute 0 file_chars, got %d", e.FileChars)
	}
	if e.SnippetChars != 5 {
		t.Errorf("snippet should still count: got %d, want 5", e.SnippetChars)
	}
}
