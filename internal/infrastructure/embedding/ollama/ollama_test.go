package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
)

// mustNewOllama constructs a Provider and fails the test if the constructor
// returns an error. Used by the happy-path tests that pass a non-empty model.
func mustNewOllama(t *testing.T, model string, opts ...ollama.Option) *ollama.Provider {
	t.Helper()
	p, err := ollama.New(model, opts...)
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}
	return p
}

func TestNew_EmptyModelReturnsTypedError(t *testing.T) {
	p, err := ollama.New("")
	if p != nil {
		t.Errorf("expected nil *Provider, got %v", p)
	}
	if !errors.Is(err, ollama.ErrMissingDependency) {
		t.Fatalf("err = %v, want wraps ErrMissingDependency", err)
	}
}

func TestModelID_ReturnsConfiguredModel(t *testing.T) {
	p := mustNewOllama(t, "nomic-embed-text")
	if got := p.ModelID(); got != "nomic-embed-text" {
		t.Fatalf("ModelID() = %q, want nomic-embed-text", got)
	}
}

func TestEmbed_HappyPath(t *testing.T) {
	want := []float32{0.1, -0.2, 0.3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Errorf("path = %s, want /api/embeddings", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "test-model" || body.Prompt != "hello" {
			t.Errorf("body = %+v, want model=test-model prompt=hello", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": want})
	}))
	defer srv.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL(srv.URL))
	got, err := p.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestEmbed_5xx_WrapsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL(srv.URL))
	_, err := p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error on 5xx")
		return
	}
	if !errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Fatalf("err = %v, want wraps ErrEmbedderUnreachable", err)
	}
}

func TestEmbed_4xx_DoesNotWrapUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad model", http.StatusBadRequest)
	}))
	defer srv.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL(srv.URL))
	_, err := p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error on 4xx")
		return
	}
	if errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Fatalf("err = %v, must NOT wrap ErrEmbedderUnreachable (caller fault)", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want status code in message", err)
	}
}

func TestEmbed_ConnectionRefused_WrapsUnreachable(t *testing.T) {
	// Bind, get an address, then close so dialing it yields a connection refused
	// (or equivalent) error.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL("http://"+addr))
	_, err = p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error on connection refused")
		return
	}
	if !errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Fatalf("err = %v, want wraps ErrEmbedderUnreachable", err)
	}
}

func TestEmbed_ContextCancel_DoesNotWrapUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{0.1}})
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Embed(ctx, "hello")
	if err == nil {
		t.Fatal("expected error on ctx cancel")
		return
	}
	if errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Fatalf("err = %v, must NOT wrap ErrEmbedderUnreachable (caller cancel)", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want ctx error", err)
	}
}

func TestEmbed_WrongShape_PlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embedding": "not-an-array"}`))
	}))
	defer srv.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL(srv.URL))
	_, err := p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected decode error")
		return
	}
	if errors.Is(err, ports.ErrEmbedderUnreachable) {
		t.Fatalf("err = %v, must NOT wrap ErrEmbedderUnreachable (server bug)", err)
	}
}

func TestEmbed_EmptyEmbedding_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embedding": []}`))
	}))
	defer srv.Close()

	p := mustNewOllama(t, "test-model", ollama.WithBaseURL(srv.URL))
	_, err := p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error on empty embedding")
		return
	}
}

// TestDefaultConstantValues pins the exported default constants so a future
// edit cannot silently drift the canonical Ollama URL or embed model
// documented in CLAUDE.md and referenced by the composition wiring layer.
func TestDefaultConstantValues(t *testing.T) {
	if ollama.DefaultBaseURL != "http://localhost:11434" {
		t.Errorf("DefaultBaseURL = %q, want %q", ollama.DefaultBaseURL, "http://localhost:11434")
	}
	if ollama.DefaultModel != "nomic-embed-text" {
		t.Errorf("DefaultModel = %q, want %q", ollama.DefaultModel, "nomic-embed-text")
	}
}
