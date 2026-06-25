// SPDX-License-Identifier: AGPL-3.0-only

package wiki

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeRenderStore is an in-memory wiki.RenderTimeStore fixture. It is
// safe for concurrent use so it can be exercised by the poller goroutine.
type fakeRenderStore struct {
	mu      sync.Mutex
	at      time.Time
	set     bool
	setCnt  int
	setErr  error
	readErr error
}

func (f *fakeRenderStore) SetLastRenderAt(_ context.Context, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.at = t
	f.set = true
	f.setCnt++
	return nil
}

func (f *fakeRenderStore) LastRenderAt(_ context.Context) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return time.Time{}, false, f.readErr
	}
	return f.at, f.set, nil
}

// fixtureHandler builds a wiki.Handler over a deterministic fixture state,
// resolving the repo root to a fresh temp dir.
func fixtureHandler(t *testing.T, store RenderTimeStore, opts ...HandlerOption) (*Handler, string) {
	t.Helper()
	root := t.TempDir()

	hz := fixtureService(t)
	ep := epFixtureService(t)

	repoRoot := func(_ context.Context, _ string) (string, error) { return root, nil }
	h, err := NewHandler(Content{HotZones: hz, EntryPoints: ep, Dependencies: fakeDeps}, store, repoRoot, opts...)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, root
}

// fakeDeps is a no-dependency DependenciesLister for handler tests.
func fakeDeps(_ context.Context, _, _ string) ([]DependencyRef, error) {
	return nil, nil
}

func TestNewHandler_RejectsNilDependencies(t *testing.T) {
	hz := fixtureService(t)
	ep := epFixtureService(t)
	store := &fakeRenderStore{}
	rr := func(_ context.Context, _ string) (string, error) { return "/tmp", nil }

	full := Content{HotZones: hz, EntryPoints: ep, Dependencies: fakeDeps}
	if _, err := NewHandler(Content{EntryPoints: ep, Dependencies: fakeDeps}, store, rr); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil hotZone: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHandler(Content{HotZones: hz, Dependencies: fakeDeps}, store, rr); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil entry: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHandler(Content{HotZones: hz, EntryPoints: ep}, store, rr); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil deps: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHandler(full, nil, rr); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil store: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHandler(full, store, nil); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil repoRoot: want ErrMissingDependency, got %v", err)
	}
}

func TestHandler_WrongKind(t *testing.T) {
	h, _ := fixtureHandler(t, &fakeRenderStore{})
	err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindEmbed})
	if err == nil {
		t.Fatal("want error on wrong kind, got nil")
		return
	}
}

// TestHandler_RegeneratesBothPages writes both wiki pages and stamps the
// render time on success.
func TestHandler_RegeneratesBothPages(t *testing.T) {
	store := &fakeRenderStore{}
	stamp := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	h, root := fixtureHandler(t, store,
		WithHandlerClock(func() time.Time { return stamp }),
		WithWritePages(true),
	)

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindWiki, RepoID: "r1", Branch: "main",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, p := range []string{HotZonesPagePath, EntryPointsPagePath, OnboardingPagePath} {
		abs := filepath.Join(root, p)
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("expected page %s to exist: %v", p, err)
		}
	}

	// AC3: render time persisted and readable.
	at, ok, err := store.LastRenderAt(context.Background())
	if err != nil {
		t.Fatalf("LastRenderAt: %v", err)
	}
	if !ok {
		t.Fatal("expected render time to be persisted")
	}
	if !at.Equal(stamp) {
		t.Errorf("render time = %v, want %v", at, stamp)
	}
}

// TestHandler_PartialFailureDoesNotStamp proves a store failure surfaces as
// a handler error and leaves no stamp (the poller will retry).
func TestHandler_StoreFailurePropagates(t *testing.T) {
	store := &fakeRenderStore{setErr: errors.New("boom")}
	h, _ := fixtureHandler(t, store)

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindWiki, RepoID: "r1", Branch: "main",
	})
	if err == nil {
		t.Fatal("want error when store fails, got nil")
		return
	}
}

// TestHandler_RegenerationUnder5s verifies AC2: both pages regenerate well
// inside the 5s budget for a typical-sized fixture graph.
func TestHandler_RegenerationUnder5s(t *testing.T) {
	h, _ := fixtureHandler(t, &fakeRenderStore{})
	start := time.Now()
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindWiki, RepoID: "r1", Branch: "main",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Errorf("regeneration took %v, want < 5s", elapsed)
	}
}
