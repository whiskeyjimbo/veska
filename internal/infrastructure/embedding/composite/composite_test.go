package composite_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/composite"
)

// fakeProvider is an EmbeddingProvider stub. err takes precedence over
// vec when both are set.
type fakeProvider struct {
	id    string
	vec   []float32
	err   error
	calls int
}

func (f *fakeProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}
func (f *fakeProvider) ModelID() string { return f.id }

// TestNew_RequiresBothProviders: a nil primary or secondary is a
// wiring error, not a runtime fallback the composite should silently
// tolerate.
func TestNew_RequiresBothProviders(t *testing.T) {
	good := &fakeProvider{id: "ok", vec: []float32{1, 0}}
	if _, err := composite.New(nil, good); err == nil {
		t.Error("nil primary: expected error")
	}
	if _, err := composite.New(good, nil); err == nil {
		t.Error("nil secondary: expected error")
	}
}

// TestEmbed_UsesPrimaryWhenAvailable pins the happy path: when the
// primary returns a vector, the secondary is never touched.
func TestEmbed_UsesPrimaryWhenAvailable(t *testing.T) {
	primary := &fakeProvider{id: "p", vec: []float32{1, 2, 3}}
	secondary := &fakeProvider{id: "s", vec: []float32{9, 9, 9}}
	c, err := composite.New(primary, secondary)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got[0] != 1 {
		t.Errorf("expected primary vector, got %v", got)
	}
	if secondary.calls != 0 {
		t.Errorf("secondary called when primary was available: calls=%d", secondary.calls)
	}
}

// TestEmbed_FallsBackOnUnreachable: only ErrEmbedderUnreachable
// triggers the fallback path — every other primary error is the
// caller's problem and must propagate so the agent learns what
// actually broke.
func TestEmbed_FallsBackOnUnreachable(t *testing.T) {
	primary := &fakeProvider{id: "p", err: fmt.Errorf("dial: %w", ports.ErrEmbedderUnreachable)}
	secondary := &fakeProvider{id: "s", vec: []float32{4, 5, 6}}
	c, _ := composite.New(primary, secondary)
	got, err := c.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got[0] != 4 {
		t.Errorf("expected secondary vector, got %v", got)
	}
	if secondary.calls != 1 {
		t.Errorf("secondary calls = %d, want 1", secondary.calls)
	}
}

// TestEmbed_DoesNotFallBackOnNonUnreachable: a 500 from Ollama is
// NOT a connectivity problem — silently switching to static would
// hide a real bug. Only the sentinel triggers fallback.
func TestEmbed_DoesNotFallBackOnNonUnreachable(t *testing.T) {
	primary := &fakeProvider{id: "p", err: errors.New("bad request")}
	secondary := &fakeProvider{id: "s", vec: []float32{4, 5, 6}}
	c, _ := composite.New(primary, secondary)
	_, err := c.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error to propagate, got nil")
	}
	if secondary.calls != 0 {
		t.Errorf("secondary called on non-unreachable error: calls=%d", secondary.calls)
	}
}

// TestEmbed_SecondaryUnreachableSurfacesAsUnreachable: when BOTH
// providers are unreachable the caller still needs the canonical
// sentinel so the search service can route to lexical fallback. We
// must wrap the secondary's error with ErrEmbedderUnreachable.
func TestEmbed_SecondaryUnreachableSurfacesAsUnreachable(t *testing.T) {
	primary := &fakeProvider{id: "p", err: fmt.Errorf("dial: %w", ports.ErrEmbedderUnreachable)}
	secondary := &fakeProvider{id: "s", err: fmt.Errorf("disk: %w", ports.ErrEmbedderUnreachable)}
	c, _ := composite.New(primary, secondary)
	_, err := c.Embed(context.Background(), "x")
	if !errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Errorf("both-down error must remain ErrEmbedderUnreachable, got %v", err)
	}
}

// TestModelID_StableAndIncludesBothIDs pins the cache-key invariant:
// the composite's ModelID must change when either underlying provider
// changes, so the embedding cache invalidates on a config swap.
func TestModelID_StableAndIncludesBothIDs(t *testing.T) {
	primary := &fakeProvider{id: "ollama"}
	secondary := &fakeProvider{id: "veska-static-v1"}
	c, _ := composite.New(primary, secondary)
	id := c.ModelID()
	if id == "" {
		t.Fatal("ModelID empty")
	}
	for _, want := range []string{"ollama", "veska-static-v1"} {
		if !contains(id, want) {
			t.Errorf("ModelID %q missing component %q", id, want)
		}
	}
	// Stable across calls — no clock / random sourcing.
	if id != c.ModelID() {
		t.Errorf("ModelID not stable")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub ||
		stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
