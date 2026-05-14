package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
)

// Compile-time interface satisfaction check.
var _ ports.LLMGenerator = (*llm.OllamaGenerator)(nil)

func TestOllamaGenerator_Generate_Success(t *testing.T) {
	t.Parallel()

	const wantText = "Hello from llama3"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/generate" {
			t.Errorf("path: got %q, want /api/generate", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"response": wantText})
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator(srv.URL, "llama3", srv.Client())
	resp, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "say hello", MaxTokens: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != wantText {
		t.Fatalf("Text: got %q, want %q", resp.Text, wantText)
	}
}

func TestOllamaGenerator_Generate_NonOKStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator(srv.URL, "llama3", srv.Client())
	_, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
}

func TestOllamaGenerator_Generate_ContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	gen := llm.NewOllamaGenerator(srv.URL, "llama3", srv.Client())
	_, err := gen.Generate(ctx, ports.GenerateRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestNewOllamaGenerator_Defaults(t *testing.T) {
	t.Parallel()
	// Ensures no panic when empty strings and nil client are passed.
	gen := llm.NewOllamaGenerator("", "", nil)
	if gen == nil {
		t.Fatal("expected non-nil generator")
	}
}
