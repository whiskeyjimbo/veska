// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/search"
)

// TestRecordSavings_PartitionsByRepo verifies that character saving recorder partitions search results by repository.
func TestRecordSavings_PartitionsByRepo(t *testing.T) {
	t.Run("single repo uses default repo id", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "savings.jsonl")
		rec, err := savings.NewRecorder(path)
		if err != nil {
			t.Fatalf("NewRecorder: %v", err)
		}
		t.Cleanup(func() { _ = rec.Close() })

		results := []search.Result{
			{NodeID: "n1", FilePath: "", Snippet: "abcde"},   // 5
			{NodeID: "n2", FilePath: "", Snippet: "fghijkl"}, // 7
		}
		recordSavings(context.Background(), rec, nil, "q", results, nil, "repo-default")
		_ = rec.Close()

		byRepo, err := savings.AggregateByRepo(path, time.Now())
		if err != nil {
			t.Fatalf("AggregateByRepo: %v", err)
		}
		if len(byRepo) != 1 {
			t.Fatalf("want 1 bucket, got %d: %+v", len(byRepo), byRepo)
		}
		rep, ok := byRepo["repo-default"]
		if !ok {
			t.Fatalf("missing repo-default bucket: %+v", byRepo)
		}
		if rep.AllTime.Calls != 1 || rep.AllTime.SnippetChars != 12 {
			t.Errorf("repo-default: %+v", rep.AllTime)
		}
	})

	t.Run("fanout writes one entry per repo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "savings.jsonl")
		rec, err := savings.NewRecorder(path)
		if err != nil {
			t.Fatalf("NewRecorder: %v", err)
		}
		t.Cleanup(func() { _ = rec.Close() })

		results := []search.Result{
			{NodeID: "n1", Snippet: "aa"},    // alpha, 2
			{NodeID: "n2", Snippet: "bbbb"},  // beta, 4
			{NodeID: "n3", Snippet: "cccc"},  // alpha, 4
			{NodeID: "n4", Snippet: "ddddd"}, // not in map → falls back to default
		}
		repoByNode := map[string]string{
			"n1": "alpha",
			"n2": "beta",
			"n3": "alpha",
		}
		recordSavings(context.Background(), rec, nil, "q", results, repoByNode, "alpha")
		_ = rec.Close()

		byRepo, err := savings.AggregateByRepo(path, time.Now())
		if err != nil {
			t.Fatalf("AggregateByRepo: %v", err)
		}
		// n4 falls back to default "alpha", so two buckets total.
		if len(byRepo) != 2 {
			t.Fatalf("want 2 buckets (alpha, beta), got %d: %+v", len(byRepo), byRepo)
		}
		if a := byRepo["alpha"].AllTime; a.Calls != 1 || a.SnippetChars != 11 {
			t.Errorf("alpha: %+v", a)
		}
		if b := byRepo["beta"].AllTime; b.Calls != 1 || b.SnippetChars != 4 {
			t.Errorf("beta: %+v", b)
		}
	})

	t.Run("empty results record nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "savings.jsonl")
		rec, err := savings.NewRecorder(path)
		if err != nil {
			t.Fatalf("NewRecorder: %v", err)
		}
		t.Cleanup(func() { _ = rec.Close() })

		recordSavings(context.Background(), rec, nil, "q", nil, nil, "repo-default")
		_ = rec.Close()

		byRepo, err := savings.AggregateByRepo(path, time.Now())
		if err != nil {
			t.Fatalf("AggregateByRepo: %v", err)
		}
		if len(byRepo) != 0 {
			t.Errorf("empty search should record nothing, got %+v", byRepo)
		}
	})
}
