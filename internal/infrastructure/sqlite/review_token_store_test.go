package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestReviewTokenStore_RoundTrip verifies that the per-day token total persists,
// accumulates, and automatically resets on a different date.
func TestReviewTokenStore_RoundTrip(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	store := sqlite.NewReviewTokenStore(db, db)
	ctx := context.Background()

	if n, err := store.TokensFor(ctx, "2026-05-17"); err != nil || n != 0 {
		t.Fatalf("TokensFor empty: n=%d err=%v, want 0", n, err)
	}

	total, err := store.AddTokens(ctx, "2026-05-17", 100)
	if err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if total != 100 {
		t.Fatalf("total = %d, want 100", total)
	}

	total, err = store.AddTokens(ctx, "2026-05-17", 250)
	if err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if total != 350 {
		t.Fatalf("total after second add = %d, want 350", total)
	}

	got, err := store.TokensFor(ctx, "2026-05-17")
	if err != nil {
		t.Fatalf("TokensFor: %v", err)
	}
	if got != 350 {
		t.Errorf("persisted total = %d, want 350", got)
	}

	// A different local date is a fresh window.
	if n, err := store.TokensFor(ctx, "2026-05-18"); err != nil || n != 0 {
		t.Errorf("TokensFor new day: n=%d err=%v, want 0", n, err)
	}
}
