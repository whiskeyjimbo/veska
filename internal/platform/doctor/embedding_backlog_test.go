// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

type fakePendingCounter struct {
	pending int
	err     error
}

func (f *fakePendingCounter) CountPending(_ context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.pending, nil
}

// TestCheckEmbeddingBacklog_Drained verifies that with no pending refs the
// backlog status is "drained" - never "healthy" (which would conflate with
// rollup status terms) and never "degraded" (the agent-facing degradation
// signal lives in eng_get_status, not doctor).
func TestCheckEmbeddingBacklog_Drained(t *testing.T) {
	t.Parallel()
	got, err := doctor.CheckEmbeddingBacklog(context.Background(), &fakePendingCounter{pending: 0})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Status != "drained" {
		t.Errorf("status: want drained, got %q (report=%+v)", got.Status, got)
	}
	if got.Pending != 0 {
		t.Errorf("pending: want 0, got %d", got.Pending)
	}
}

// TestCheckEmbeddingBacklog_Backfilling verifies that any pending count >0
// classifies as "backfilling". The threshold is 0/non-0 is
// about reconciling presentation with eng_get_status, which sets
// embeddings_pending whenever pending>0.
func TestCheckEmbeddingBacklog_Backfilling(t *testing.T) {
	t.Parallel()
	got, err := doctor.CheckEmbeddingBacklog(context.Background(), &fakePendingCounter{pending: 6480})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Status != "backfilling" {
		t.Errorf("status: want backfilling, got %q", got.Status)
	}
	if got.Pending != 6480 {
		t.Errorf("pending: want 6480, got %d", got.Pending)
	}
}

// TestCheckEmbeddingBacklog_QueryError reports "unknown" so the rollup
// shows we couldn't measure it - but never "degraded" or "broken", because
// a backlog query failure is not itself a real fault (it could be the
// daemon holding the write lock, etc).
func TestCheckEmbeddingBacklog_QueryError(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbeddingBacklog(context.Background(), &fakePendingCounter{err: errors.New("boom")})
	if got.Status != "unknown" {
		t.Errorf("status: want unknown on query error, got %q", got.Status)
	}
}

// TestCheckEmbeddingBacklog_NilCounter degrades gracefully when no counter
// is wired (test/legacy callers).
func TestCheckEmbeddingBacklog_NilCounter(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckEmbeddingBacklog(context.Background(), nil)
	if got.Status != "unknown" {
		t.Errorf("status: want unknown on nil counter, got %q", got.Status)
	}
}
