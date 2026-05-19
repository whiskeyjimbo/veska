package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// usageGenerator is a fakeGenerator that reports a fixed token usage per call.
type usageGenerator struct {
	mu      sync.Mutex
	calls   int
	perCall int
	reply   string
}

func (g *usageGenerator) Generate(_ context.Context, _ ports.GenerateRequest) (ports.GenerateResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return ports.GenerateResponse{
		Text:  g.reply,
		Usage: ports.TokenUsage{PromptTokens: g.perCall / 2, CompletionTokens: g.perCall - g.perCall/2},
	}, nil
}

func (g *usageGenerator) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// fakeAuditWriter records every audit entry. Safe for concurrent use.
type fakeAuditWriter struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (w *fakeAuditWriter) Write(_ context.Context, e ports.AuditEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, e)
	return nil
}

func (w *fakeAuditWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entries)
}

// fakeErrorCounter records review-error increments.
type fakeErrorCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newFakeErrorCounter() *fakeErrorCounter {
	return &fakeErrorCounter{counts: make(map[string]int)}
}

func (c *fakeErrorCounter) IncError(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[kind]++
}

func (c *fakeErrorCounter) get(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[kind]
}

func writeReviewFile(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

// TestHandler_CommitOverageRefusesRemaining verifies AC1: once a commit's
// running total reaches max_tokens_per_commit, the NEXT review job for that
// commit is refused (no LLM call) and exactly one budget-exceeded finding is
// filed; the refusal increments the review error metric.
func TestHandler_CommitOverageRefusesRemaining(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReviewFile(t, root)

	loader, _ := NewLoader()
	// Each Generate call reports a large token usage so the first job's
	// kinds blow the per-commit cap.
	gen := &usageGenerator{reply: `{"findings":[]}`, perCall: 10_000}
	fs := &fakeFindingStorage{}
	metrics := newFakeErrorCounter()
	q := NewQuota(100, 0, newFakeDailyStore(), nil)

	h, err := NewHandler(gen, loader, staticRoot(root), fs,
		WithQuota(q), WithAuditWriter(&fakeAuditWriter{}), WithErrorCounter(metrics))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	row := ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main",
		GitSHA: "sha-budget", Payload: "a.go",
	}
	// First job runs (crosses the line) — succeeds.
	if err := h.Handle(context.Background(), row); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	callsAfterFirst := gen.callCount()
	if callsAfterFirst == 0 {
		t.Fatal("first job should have called the LLM")
	}

	// Second job for the SAME commit must be refused: no LLM call.
	err = h.Handle(context.Background(), row)
	if err == nil {
		t.Fatal("second job should be refused with an error")
		return
	}
	if !strings.Contains(err.Error(), quotaExceeded) {
		t.Errorf("refusal error = %q, want it to carry %q", err.Error(), quotaExceeded)
	}
	if gen.callCount() != callsAfterFirst {
		t.Errorf("refused job still called the LLM: calls %d -> %d", callsAfterFirst, gen.callCount())
	}

	// Exactly one budget-exceeded finding, severity medium, anchored on commit.
	var budget []*domain.Finding
	for _, f := range fs.findings() {
		if f.Rule == BudgetRule {
			budget = append(budget, f)
		}
	}
	if len(budget) != 1 {
		t.Fatalf("budget-exceeded findings = %d, want exactly 1", len(budget))
	}
	if budget[0].Severity != domain.SeverityMedium {
		t.Errorf("severity = %q, want medium", budget[0].Severity)
	}
	if budget[0].NodeID == nil || *budget[0].NodeID != "sha-budget" {
		t.Errorf("anchor = %v, want sha-budget", budget[0].NodeID)
	}
	if metrics.get("review") == 0 {
		t.Error("refusal must increment veska_error_count{kind=review}")
	}

	// A third refused job must NOT file another finding (idempotent).
	_ = h.Handle(context.Background(), row)
	budget = budget[:0]
	for _, f := range fs.findings() {
		if f.Rule == BudgetRule {
			budget = append(budget, f)
		}
	}
	if len(budget) != 1 {
		t.Errorf("budget-exceeded findings after 3rd job = %d, want still 1", len(budget))
	}
}

// TestHandler_CommitCapZeroUnlimited proves a per-commit cap of 0 never
// refuses a job.
func TestHandler_CommitCapZeroUnlimited(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReviewFile(t, root)
	loader, _ := NewLoader()
	gen := &usageGenerator{reply: `{"findings":[]}`, perCall: 10_000_000}
	fs := &fakeFindingStorage{}
	q := NewQuota(0, 0, newFakeDailyStore(), nil)
	h, _ := NewHandler(gen, loader, staticRoot(root), fs, WithQuota(q))

	row := ports.WorkRow{Kind: ports.WorkKindReview, RepoID: "r", Branch: "main", GitSHA: "s", Payload: "a.go"}
	for i := range 3 {
		if err := h.Handle(context.Background(), row); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
}

// TestHandler_DailyCapPauses verifies AC2: when the daily total has reached
// max_tokens_per_day the handler pauses — no LLM call — and writes exactly one
// audit.jsonl line the first time the pause trips. No finding is filed.
func TestHandler_DailyCapPauses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReviewFile(t, root)
	loader, _ := NewLoader()
	gen := &usageGenerator{reply: `{"findings":[]}`, perCall: 100}
	fs := &fakeFindingStorage{}
	audit := &fakeAuditWriter{}
	store := newFakeDailyStore()
	// Pre-seed the day at the cap.
	_, _ = store.AddTokens(context.Background(), NewQuota(0, 0, store, nil).localDate(), 500)
	q := NewQuota(0, 500, store, nil)
	h, _ := NewHandler(gen, loader, staticRoot(root), fs, WithQuota(q), WithAuditWriter(audit))

	row := ports.WorkRow{Kind: ports.WorkKindReview, RepoID: "r", Branch: "main", GitSHA: "s", Payload: "a.go"}
	if err := h.Handle(context.Background(), row); err == nil {
		t.Fatal("paused handler should return an error, got nil")
		return
	}
	if gen.callCount() != 0 {
		t.Errorf("paused handler called the LLM %d times, want 0", gen.callCount())
	}
	if audit.count() != 1 {
		t.Errorf("audit lines = %d, want exactly 1 on first pause trip", audit.count())
	}
	// A second paused job must not write another audit line.
	_ = h.Handle(context.Background(), row)
	if audit.count() != 1 {
		t.Errorf("audit lines after 2nd paused job = %d, want still 1", audit.count())
	}
	// No finding for the daily-cap pause.
	for _, f := range fs.findings() {
		if f.Rule == BudgetRule {
			t.Error("daily-cap pause must not file a budget-exceeded finding")
		}
	}
}

// TestHandler_NoQuotaBehavesAsBefore proves a Handler built without a quota
// runs jobs unconditionally (backward compatible).
func TestHandler_NoQuotaBehavesAsBefore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeReviewFile(t, root)
	loader, _ := NewLoader()
	gen := &usageGenerator{reply: `{"findings":[]}`, perCall: 10_000_000}
	h, _ := NewHandler(gen, loader, staticRoot(root), &fakeFindingStorage{})
	row := ports.WorkRow{Kind: ports.WorkKindReview, RepoID: "r", Branch: "main", GitSHA: "s", Payload: "a.go"}
	if err := h.Handle(context.Background(), row); err != nil {
		t.Fatalf("Handle without quota: %v", err)
	}
}
