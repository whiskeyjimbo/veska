package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/savings"
)

// TestRunSavings_NoDataMessage: when the jsonl file is absent (fresh
// install, daemon never ran), the doctor subcommand prints a friendly
// "no calls recorded" line instead of an empty zero-bar chart.
func TestRunSavings_NoDataMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := runSavings(&buf, t.TempDir(), time.Now(), false, false); err != nil {
		t.Fatalf("runSavings: %v", err)
	}
	if !strings.Contains(buf.String(), "no search calls recorded") {
		t.Errorf("expected no-data message, got: %q", buf.String())
	}
}

// TestRunSavings_RendersBarsAndPercentages: with real entries on disk
// the renderer must emit three rows (today, last_7d, all_time), each
// with a bar and the percentage that matches the underlying ratio.
func TestRunSavings_RendersBarsAndPercentages(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "savings.jsonl")

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Use savingsMinSampleCalls entries so the percent renders (solov2-qjhg);
	// below that threshold the row reads "warming up" and intentionally hides
	// noisy small-sample ratios.
	entries := make([]savings.Entry, savingsMinSampleCalls)
	for i := range entries {
		entries[i] = savings.Entry{
			Timestamp:    now.Add(-1 * time.Hour),
			Query:        "q",
			FileChars:    10000,
			SnippetChars: 200,
			Results:      1,
		}
	}
	f, err := os.Create(jsonl)
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

	var buf bytes.Buffer
	if err := runSavings(&buf, dir, now, false, false); err != nil {
		t.Fatalf("runSavings: %v", err)
	}
	out := buf.String()
	// Today row with 98.0% savings (1 - 200/10000).
	if !strings.Contains(out, "today") || !strings.Contains(out, "98.0%") {
		t.Errorf("expected today row at 98.0%%, got:\n%s", out)
	}
	if !strings.Contains(out, "all_time") {
		t.Errorf("missing all_time row: %s", out)
	}
}

// TestRunSavings_JSONFlag: --json flag round-trips a savings.Report
// shape, not the human-rendered text.
func TestRunSavings_JSONFlag(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "savings.jsonl")
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	e := savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1}
	f, _ := os.Create(jsonl)
	_ = json.NewEncoder(f).Encode(e)
	_ = f.Close()

	var buf bytes.Buffer
	if err := runSavings(&buf, dir, now, true, false); err != nil {
		t.Fatalf("runSavings json: %v", err)
	}
	var got savings.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal report: %v (raw=%s)", err, buf.String())
	}
	if got.Today.Calls != 1 || got.Today.FileChars != 100 {
		t.Errorf("today period wrong: %+v", got.Today)
	}
}

// TestRunSavings_AllReposLabel: until the recorder is partitioned by
// repo_id (solov2-0ql0), the text renderer labels its single bucket as
// "all repos" so the user knows the figure is pooled across every
// registered repo, not specific to one.
func TestRunSavings_AllReposLabel(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "savings.jsonl")
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	e := savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1}
	f, _ := os.Create(jsonl)
	_ = json.NewEncoder(f).Encode(e)
	_ = f.Close()

	var buf bytes.Buffer
	if err := runSavings(&buf, dir, now, false, false); err != nil {
		t.Fatalf("runSavings: %v", err)
	}
	if !strings.Contains(buf.String(), "all repos") {
		t.Errorf("expected 'all repos' bucket label, got:\n%s", buf.String())
	}
}

// TestRunSavings_AggregateFlag: --aggregate forces the pooled single-row
// output. Today it is the only output mode (per-repo split is gated on
// solov2-0ql0); the flag exists now so the eventual per-repo default
// has a documented opt-out, and so 'veska savings --aggregate' is a
// stable command for users who script against it.
func TestRunSavings_AggregateFlag(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "savings.jsonl")
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	e := savings.Entry{Timestamp: now, Query: "q", FileChars: 100, SnippetChars: 10, Results: 1}
	f, _ := os.Create(jsonl)
	_ = json.NewEncoder(f).Encode(e)
	_ = f.Close()

	var buf bytes.Buffer
	if err := runSavings(&buf, dir, now, false, true); err != nil {
		t.Fatalf("runSavings: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "today") || !strings.Contains(out, "all_time") {
		t.Errorf("aggregate output missing standard rows: %s", out)
	}
}
