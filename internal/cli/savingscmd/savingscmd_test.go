package savingscmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/savings"
)

// testBytes is a stand-in for cmd/veska's humanBytes; the renderer only needs
// some deterministic formatter.
func testBytes(n int64) string { return fmt.Sprintf("%dB", n) }

func params(out *bytes.Buffer, home string, now time.Time) Params {
	return Params{Out: out, VeskaHome: home, Now: now, FormatBytes: testBytes}
}

func writeEntries(t *testing.T, dir string, entries ...savings.Entry) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "savings.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
}

// When the jsonl file is absent (fresh install, daemon never ran), Run prints a
// friendly "no calls recorded" line instead of an empty zero-bar chart.
func TestRun_NoDataMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := Run(params(&buf, t.TempDir(), time.Now())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "no search calls recorded") {
		t.Errorf("expected no-data message, got: %q", buf.String())
	}
}

// With real entries on disk the renderer emits three rows (today, last_7d,
// all_time), each with a bar and a percentage matching the underlying ratio.
func TestRun_RendersBarsAndPercentages(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Use minSampleCalls entries so the percent renders ; below
	// that threshold the row reads "warming up".
	entries := make([]savings.Entry, minSampleCalls)
	for i := range entries {
		entries[i] = savings.Entry{Timestamp: now.Add(-1 * time.Hour), Query: "q", FileChars: 10000, SnippetChars: 200, Results: 1}
	}
	writeEntries(t, dir, entries...)

	var buf bytes.Buffer
	if err := Run(params(&buf, dir, now)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "today") || !strings.Contains(out, "98.0%") {
		t.Errorf("expected today row at 98.0%%, got:\n%s", out)
	}
	if !strings.Contains(out, "all_time") {
		t.Errorf("missing all_time row: %s", out)
	}
}

// --json round-trips a savings.Report shape, not the human-rendered text.
func TestRun_JSONFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir, savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1})

	p := params(&bytes.Buffer{}, dir, now)
	buf := p.Out.(*bytes.Buffer)
	p.JSON = true
	if err := Run(p); err != nil {
		t.Fatalf("Run json: %v", err)
	}
	var got savings.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal report: %v (raw=%s)", err, buf.String())
	}
	if got.Today.Calls != 1 || got.Today.FileChars != 100 {
		t.Errorf("today period wrong: %+v", got.Today)
	}
}

// Until the recorder is partitioned by repo_id , the text renderer
// labels its single bucket "all repos" so the user knows the figure is pooled.
func TestRun_AllReposLabel(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir, savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1})

	var buf bytes.Buffer
	if err := Run(params(&buf, dir, now)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "all repos") {
		t.Errorf("expected 'all repos' bucket label, got:\n%s", buf.String())
	}
}

// --aggregate forces the pooled single-row output (the only mode today).
func TestRun_AggregateFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir, savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1})

	p := params(&bytes.Buffer{}, dir, now)
	buf := p.Out.(*bytes.Buffer)
	p.Aggregate = true
	if err := Run(p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "today") || !strings.Contains(out, "all_time") {
		t.Errorf("aggregate output missing standard rows: %s", out)
	}
}
