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

// Default --json groups counters by repo_id (solov2-izh6.21) and carries a
// pooled total. Legacy entries with no repo_id land under the "" key.
func TestRun_JSONFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir,
		savings.Entry{Timestamp: now, RepoID: "alpha", Query: "q", FileChars: 100, SnippetChars: 10, Results: 1},
		savings.Entry{Timestamp: now, RepoID: "beta", Query: "q", FileChars: 200, SnippetChars: 20, Results: 1},
	)

	p := params(&bytes.Buffer{}, dir, now)
	buf := p.Out.(*bytes.Buffer)
	p.JSON = true
	if err := Run(p); err != nil {
		t.Fatalf("Run json: %v", err)
	}
	var got repoBreakdown
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal breakdown: %v (raw=%s)", err, buf.String())
	}
	if a := got.Repos["alpha"].Today; a.Calls != 1 || a.FileChars != 100 {
		t.Errorf("alpha today wrong: %+v", a)
	}
	if b := got.Repos["beta"].Today; b.Calls != 1 || b.FileChars != 200 {
		t.Errorf("beta today wrong: %+v", b)
	}
	if got.Total.Today.Calls != 2 || got.Total.Today.FileChars != 300 {
		t.Errorf("total today wrong: %+v", got.Total.Today)
	}
}

// --aggregate --json preserves the pre-breakdown savings.Report shape so
// scripts that parsed the pooled JSON keep working.
func TestRun_AggregateJSONFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir, savings.Entry{Timestamp: now, RepoID: "alpha", Query: "q", FileChars: 100, SnippetChars: 10, Results: 1})

	p := params(&bytes.Buffer{}, dir, now)
	buf := p.Out.(*bytes.Buffer)
	p.JSON = true
	p.Aggregate = true
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

// --aggregate labels its single pooled bucket "all repos" so the user knows
// the figure spans every repo.
func TestRun_AllReposLabel(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir, savings.Entry{Timestamp: now, RepoID: "alpha", Query: "q", FileChars: 100, SnippetChars: 10, Results: 1})

	p := params(&bytes.Buffer{}, dir, now)
	buf := p.Out.(*bytes.Buffer)
	p.Aggregate = true
	if err := Run(p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "all repos") {
		t.Errorf("expected 'all repos' bucket label, got:\n%s", buf.String())
	}
}

// Default text mode prints one section per repo (most-active first) plus a
// pooled total (solov2-izh6.21).
func TestRun_PerRepoSections(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// beta searched more often than alpha → beta sorts first.
	entries := []savings.Entry{
		{Timestamp: now, RepoID: "alpha111111111", Query: "q", FileChars: 100, SnippetChars: 10, Results: 1},
		{Timestamp: now, RepoID: "beta2222222222", Query: "q", FileChars: 200, SnippetChars: 20, Results: 1},
		{Timestamp: now, RepoID: "beta2222222222", Query: "q", FileChars: 200, SnippetChars: 20, Results: 1},
	}
	writeEntries(t, dir, entries...)

	var buf bytes.Buffer
	if err := Run(params(&buf, dir, now)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "beta22222222") || !strings.Contains(out, "alpha1111111") {
		t.Errorf("expected short repo ids in output, got:\n%s", out)
	}
	if !strings.Contains(out, "total:") {
		t.Errorf("expected total section, got:\n%s", out)
	}
	// Most-active repo (beta) section appears before the least-active (alpha).
	if bi, ai := strings.Index(out, "beta22222222"), strings.Index(out, "alpha1111111"); bi < 0 || ai < 0 || bi > ai {
		t.Errorf("expected beta before alpha (most-active first), got:\n%s", out)
	}
}

// Legacy entries with no repo_id render under an explicit "(untagged)" header
// in the per-repo view rather than a blank section.
func TestRun_UntaggedSection(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	writeEntries(t, dir, savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1})

	var buf bytes.Buffer
	if err := Run(params(&buf, dir, now)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "(untagged)") {
		t.Errorf("expected '(untagged)' label for repo-less entries, got:\n%s", buf.String())
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
