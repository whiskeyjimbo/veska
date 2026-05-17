package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestWikiRenderStateRepo_RoundTrip verifies the last-render timestamp is
// persisted to daemon_state and read back (AC3).
func TestWikiRenderStateRepo_RoundTrip(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	repo := sqlite.NewWikiRenderStateRepo(db, db)
	ctx := context.Background()

	// No render recorded yet.
	if _, ok, err := repo.LastRenderAt(ctx); err != nil || ok {
		t.Fatalf("LastRenderAt before any render: ok=%v err=%v, want ok=false", ok, err)
	}

	want := time.UnixMilli(time.Now().UnixMilli())
	if err := repo.SetLastRenderAt(ctx, want); err != nil {
		t.Fatalf("SetLastRenderAt: %v", err)
	}
	got, ok, err := repo.LastRenderAt(ctx)
	if err != nil {
		t.Fatalf("LastRenderAt: %v", err)
	}
	if !ok {
		t.Fatal("expected render time to be persisted")
	}
	if !got.Equal(want) {
		t.Errorf("render time = %v, want %v", got, want)
	}

	// A second write upserts (no PK conflict error).
	later := want.Add(time.Hour)
	if err := repo.SetLastRenderAt(ctx, later); err != nil {
		t.Fatalf("SetLastRenderAt (upsert): %v", err)
	}
	got, _, err = repo.LastRenderAt(ctx)
	if err != nil {
		t.Fatalf("LastRenderAt after upsert: %v", err)
	}
	if !got.Equal(later) {
		t.Errorf("render time after upsert = %v, want %v", got, later)
	}
}
