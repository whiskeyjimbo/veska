package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeGenerator is an in-memory ports.LLMGenerator fixture. It records every
// request it receives and is safe for concurrent use so it can be exercised
// from the poller goroutine.
type fakeGenerator struct {
	mu    sync.Mutex
	reqs  []ports.GenerateRequest
	reply string
	err   error
}

func (f *fakeGenerator) Generate(_ context.Context, req ports.GenerateRequest) (ports.GenerateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return ports.GenerateResponse{}, f.err
	}
	return ports.GenerateResponse{Text: f.reply}, nil
}

func (f *fakeGenerator) calls() []ports.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.GenerateRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// staticRoot returns a RepoRootFunc resolving every repoID to root.
func staticRoot(root string) RepoRootFunc {
	return func(_ context.Context, _ string) (string, error) { return root, nil }
}

// TestNewHandler_NilDependency proves a nil collaborator wraps
// ErrMissingDependency and yields a nil handler.
func TestNewHandler_NilDependency(t *testing.T) {
	t.Parallel()
	loader, err := NewLoader()
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	cases := map[string]func() (*Handler, error){
		"nil gen":      func() (*Handler, error) { return NewHandler(nil, loader, staticRoot("/tmp")) },
		"nil loader":   func() (*Handler, error) { return NewHandler(&fakeGenerator{}, nil, staticRoot("/tmp")) },
		"nil repoRoot": func() (*Handler, error) { return NewHandler(&fakeGenerator{}, loader, nil) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, err := build()
			if !errors.Is(err, ErrMissingDependency) {
				t.Fatalf("err = %v, want ErrMissingDependency", err)
			}
			if h != nil {
				t.Errorf("handler = %v, want nil", h)
			}
		})
	}
}

// TestHandler_WrongKind proves a misrouted row returns a wrapped error.
func TestHandler_WrongKind(t *testing.T) {
	t.Parallel()
	loader, _ := NewLoader()
	h, err := NewHandler(&fakeGenerator{}, loader, staticRoot(t.TempDir()))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindWiki}); err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

// TestHandler_DispatchesThroughGenerator verifies AC2: the lane renders the
// loaded review prompt, calls Generate with the prompt template version, and
// parses the response.
func TestHandler_DispatchesThroughGenerator(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	loader, err := NewLoader()
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	gen := &fakeGenerator{reply: "NO FINDINGS"}
	h, err := NewHandler(gen, loader, staticRoot(root))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	calls := gen.calls()
	if len(calls) != len(loader.Kinds()) {
		t.Fatalf("Generate called %d times, want %d (one per kind)", len(calls), len(loader.Kinds()))
	}
	for _, c := range calls {
		if c.Prompt == "" {
			t.Error("Generate called with empty rendered prompt")
		}
		if c.PromptTemplateVersion == "" {
			t.Error("Generate called without PromptTemplateVersion")
		}
	}
}

// TestHandler_EmptyPayload proves a row with no file path drains cleanly.
func TestHandler_EmptyPayload(t *testing.T) {
	t.Parallel()
	loader, _ := NewLoader()
	gen := &fakeGenerator{}
	h, _ := NewHandler(gen, loader, staticRoot(t.TempDir()))
	if err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindReview}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gen.calls()) != 0 {
		t.Errorf("Generate called %d times for empty payload, want 0", len(gen.calls()))
	}
}

// TestHandler_GeneratorErrorPropagates proves an LLM failure surfaces as a
// wrapped error so the poller's retry path runs.
func TestHandler_GeneratorErrorPropagates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	sentinel := errors.New("model down")
	h, _ := NewHandler(&fakeGenerator{err: sentinel}, loader, staticRoot(root))
	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped model-down error", err)
	}
}
