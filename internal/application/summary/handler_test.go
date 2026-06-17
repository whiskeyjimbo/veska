package summary

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeGen is a scripted LLMGenerator. It returns respText (or err) and records
// every prompt it saw.
type fakeGen struct {
	respText string
	err      error
	prompts  []string
}

func (g *fakeGen) Generate(_ context.Context, req ports.GenerateRequest) (ports.GenerateResponse, error) {
	g.prompts = append(g.prompts, req.Prompt)
	if g.err != nil {
		return ports.GenerateResponse{}, g.err
	}
	return ports.GenerateResponse{Text: g.respText}, nil
}

// fakeStore returns a fixed node set and captures SetShortSummary writes.
type fakeStore struct {
	nodes   []Node
	written map[string]string
	setErr  error
}

func (s *fakeStore) PromotedNodes(_ context.Context, _, _, _ string) ([]Node, error) {
	return s.nodes, nil
}

func (s *fakeStore) SetShortSummary(_ context.Context, _, _, nodeID, summary string) error {
	if s.setErr != nil {
		return s.setErr
	}
	if s.written == nil {
		s.written = map[string]string{}
	}
	s.written[nodeID] = summary
	return nil
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rootFor(dir string) RepoRootFunc {
	return func(_ context.Context, _ string) (string, error) { return dir, nil }
}

func row(file string) ports.WorkRow {
	return ports.WorkRow{Kind: ports.WorkKindSummary, RepoID: "r", Branch: "main", Payload: file}
}

func TestHandle_PersistsParsedSummary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x.go", "package x\n\nfunc Foo() {}\n")

	gen := &fakeGen{respText: `{"summary": "adds two integers"}`}
	store := &fakeStore{nodes: []Node{{NodeID: "n1", Kind: "function", Name: "Foo", LineStart: 3, LineEnd: 3}}}

	h, err := NewHandler(gen, store, rootFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(context.Background(), row("x.go")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.written["n1"]; got != "adds two integers" {
		t.Fatalf("stored summary = %q, want %q", got, "adds two integers")
	}
}

func TestHandle_SkipsContainerKinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x.go", "package x\n")

	gen := &fakeGen{respText: `{"summary": "should not be called"}`}
	store := &fakeStore{nodes: []Node{
		{NodeID: "pkg", Kind: "package", Name: "x", LineStart: 1, LineEnd: 1},
		{NodeID: "fld", Kind: "field", Name: "y", LineStart: 1, LineEnd: 1},
	}}

	h, _ := NewHandler(gen, store, rootFor(dir))
	if err := h.Handle(context.Background(), row("x.go")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gen.prompts) != 0 {
		t.Fatalf("generator called %d times for container-only file, want 0", len(gen.prompts))
	}
	if len(store.written) != 0 {
		t.Fatalf("wrote %d summaries for container-only file, want 0", len(store.written))
	}
}

func TestHandle_EmptyModelOutputFallsBackToHeuristic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x.go", "package x\nfunc Foo() {}\n")

	gen := &fakeGen{respText: "not json at all"}
	sig := "func Foo()"
	store := &fakeStore{nodes: []Node{{NodeID: "n1", Kind: "function", Name: "Foo", Signature: sig, LineStart: 2, LineEnd: 2}}}

	h, _ := NewHandler(gen, store, rootFor(dir))
	if err := h.Handle(context.Background(), row("x.go")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := store.written["n1"]; got != sig {
		t.Fatalf("fallback summary = %q, want heuristic %q", got, sig)
	}
}

func TestHandle_GeneratorErrorRequeues(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x.go", "package x\nfunc Foo() {}\n")

	gen := &fakeGen{err: errors.New("ollama down")}
	store := &fakeStore{nodes: []Node{{NodeID: "n1", Kind: "function", Name: "Foo", LineStart: 2, LineEnd: 2}}}

	h, _ := NewHandler(gen, store, rootFor(dir))
	err := h.Handle(context.Background(), row("x.go"))
	if err == nil {
		t.Fatal("expected error so the poller re-queues, got nil")
	}
	if len(store.written) != 0 {
		t.Fatalf("should not persist on generator failure, wrote %d", len(store.written))
	}
}

func TestHandle_WrongKindIsError(t *testing.T) {
	h, _ := NewHandler(&fakeGen{}, &fakeStore{}, rootFor(t.TempDir()))
	err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindReview, Payload: "x.go"})
	if err == nil || !strings.Contains(err.Error(), "unexpected kind") {
		t.Fatalf("want unexpected-kind error, got %v", err)
	}
}

func TestNewHandler_MissingDeps(t *testing.T) {
	if _, err := NewHandler(nil, &fakeStore{}, rootFor("")); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("nil gen: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHandler(&fakeGen{}, nil, rootFor("")); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("nil store: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHandler(&fakeGen{}, &fakeStore{}, nil); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("nil root: want ErrMissingDependency, got %v", err)
	}
}
