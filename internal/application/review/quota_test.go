package review

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeDailyStore is an in-memory DailyTokenStore keyed by local date. It is
// safe for concurrent use so the race detector can exercise the quota.
type fakeDailyStore struct {
	mu   sync.Mutex
	days map[string]int
}

func newFakeDailyStore() *fakeDailyStore {
	return &fakeDailyStore{days: make(map[string]int)}
}

func (f *fakeDailyStore) TokensFor(_ context.Context, date string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.days[date], nil
}

func (f *fakeDailyStore) AddTokens(_ context.Context, date string, n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.days[date] += n
	return f.days[date], nil
}

// TestQuota_CommitCap proves the per-commit running total trips the cap.
func TestQuota_CommitCap(t *testing.T) {
	t.Parallel()
	q := NewQuota(100, 0, newFakeDailyStore(), nil)
	ctx := context.Background()

	if q.CommitExceeded("sha1") {
		t.Fatal("commit should not be exceeded with zero usage")
	}
	if err := q.Record(ctx, "sha1", 60); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if q.CommitExceeded("sha1") {
		t.Fatal("commit should not be exceeded at 60/100")
	}
	if err := q.Record(ctx, "sha1", 50); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !q.CommitExceeded("sha1") {
		t.Fatal("commit should be exceeded at 110/100")
	}
	if q.CommitExceeded("sha2") {
		t.Fatal("unrelated commit must not be affected")
	}
}

// TestQuota_CommitCapZeroUnlimited proves a cap of 0 disables the per-commit
// check entirely — it never trips no matter the usage.
func TestQuota_CommitCapZeroUnlimited(t *testing.T) {
	t.Parallel()
	q := NewQuota(0, 0, newFakeDailyStore(), nil)
	if err := q.Record(context.Background(), "sha1", 1_000_000); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if q.CommitExceeded("sha1") {
		t.Fatal("a cap of 0 means unlimited; commit must never be exceeded")
	}
}

// TestQuota_DailyCapAndMidnightReset verifies AC2: the daily total trips the
// pause once the cap is reached, and the window resets at local midnight
// (driven by an injected clock).
func TestQuota_DailyCapAndMidnightReset(t *testing.T) {
	t.Parallel()
	day1 := time.Date(2026, 5, 17, 10, 0, 0, 0, time.Local)
	clock := day1
	q := NewQuota(0, 1000, newFakeDailyStore(), func() time.Time { return clock })
	ctx := context.Background()

	paused, total, err := q.DailyPaused(ctx)
	if err != nil {
		t.Fatalf("DailyPaused: %v", err)
	}
	if paused || total != 0 {
		t.Fatalf("paused=%v total=%d, want false/0", paused, total)
	}

	if err := q.Record(ctx, "sha1", 1000); err != nil {
		t.Fatalf("Record: %v", err)
	}
	paused, total, err = q.DailyPaused(ctx)
	if err != nil {
		t.Fatalf("DailyPaused: %v", err)
	}
	if !paused || total != 1000 {
		t.Fatalf("paused=%v total=%d, want true/1000", paused, total)
	}

	// Advance the clock past local midnight: the window resets.
	clock = day1.AddDate(0, 0, 1)
	paused, total, err = q.DailyPaused(ctx)
	if err != nil {
		t.Fatalf("DailyPaused: %v", err)
	}
	if paused || total != 0 {
		t.Fatalf("after midnight: paused=%v total=%d, want false/0", paused, total)
	}
}

// TestQuota_DailyCapZeroUnlimited proves a daily cap of 0 never pauses.
func TestQuota_DailyCapZeroUnlimited(t *testing.T) {
	t.Parallel()
	q := NewQuota(0, 0, newFakeDailyStore(), nil)
	if err := q.Record(context.Background(), "sha1", 1_000_000); err != nil {
		t.Fatalf("Record: %v", err)
	}
	paused, _, err := q.DailyPaused(context.Background())
	if err != nil {
		t.Fatalf("DailyPaused: %v", err)
	}
	if paused {
		t.Fatal("a daily cap of 0 means unlimited; must never pause")
	}
}

// TestQuota_TokensToday reports the persisted daily total for AC3.
func TestQuota_TokensToday(t *testing.T) {
	t.Parallel()
	q := NewQuota(0, 5000, newFakeDailyStore(), nil)
	ctx := context.Background()
	if err := q.Record(ctx, "sha1", 1200); err != nil {
		t.Fatalf("Record: %v", err)
	}
	tokens, err := q.TokensToday(ctx)
	if err != nil {
		t.Fatalf("TokensToday: %v", err)
	}
	if tokens != 1200 {
		t.Fatalf("tokens_today = %d, want 1200", tokens)
	}
}
