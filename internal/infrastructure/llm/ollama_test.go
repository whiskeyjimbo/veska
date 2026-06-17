package llm_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
)

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

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	resp, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "say hello", MaxTokens: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != wantText {
		t.Fatalf("Text: got %q, want %q", resp.Text, wantText)
	}
}

// A successful Generate returns provenance containing model id, prompt-template
// version, and an input hash.
func TestOllamaGenerator_Generate_Provenance(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"response": "ok"})
	}))
	defer srv.Close()

	const prompt = "review this commit"
	const tmplVer = "review/v3"

	gen := llm.NewOllamaGenerator("llama3.1:8b", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	resp, err := gen.Generate(context.Background(), ports.GenerateRequest{
		Prompt:                prompt,
		PromptTemplateVersion: tmplVer,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Provenance.ModelID != "llama3.1:8b" {
		t.Errorf("ModelID: got %q, want %q", resp.Provenance.ModelID, "llama3.1:8b")
	}
	if resp.Provenance.PromptTemplateVersion != tmplVer {
		t.Errorf("PromptTemplateVersion: got %q, want %q", resp.Provenance.PromptTemplateVersion, tmplVer)
	}
	sum := sha256.Sum256([]byte(prompt))
	wantHash := hex.EncodeToString(sum[:])
	if resp.Provenance.InputHash != wantHash {
		t.Errorf("InputHash: got %q, want %q", resp.Provenance.InputHash, wantHash)
	}
}

// When GenerateRequest.Format is set, OllamaGenerator forwards it as the
// /api/generate 'format' parameter so the model is constrained to schema-valid JSON.
func TestOllamaGenerator_Generate_StructuredFormat(t *testing.T) {
	t.Parallel()

	schema := json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"}}}`)

	var gotFormat json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Format json.RawMessage `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		gotFormat = body.Format
		_ = json.NewEncoder(w).Encode(map[string]string{"response": `{"findings":[]}`})
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	if _, err := gen.Generate(context.Background(), ports.GenerateRequest{
		Prompt: "review this",
		Format: schema,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got, want any
	if err := json.Unmarshal(gotFormat, &got); err != nil {
		t.Fatalf("request had no/invalid 'format' field: %q: %v", gotFormat, err)
	}
	if err := json.Unmarshal(schema, &want); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("format: got %v, want %v", got, want)
	}
}

// A plain-text GenerateRequest (empty Format) omits the 'format' field to ensure
// standard plain-text generation is unaffected.
func TestOllamaGenerator_Generate_NoFormatByDefault(t *testing.T) {
	t.Parallel()

	var hasFormat bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		_, hasFormat = raw["format"]
		_ = json.NewEncoder(w).Encode(map[string]string{"response": "ok"})
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	if _, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasFormat {
		t.Error("plain-text request unexpectedly carried a 'format' field")
	}
}

// A transient HTTP 5xx error triggers a retry up to the maximum attempts count.
func TestOllamaGenerator_Generate_RetriesTransient(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			http.Error(w, "model loading", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"response": "recovered"})
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()),
		llm.WithBackoff(time.Millisecond))
	resp, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "recovered" {
		t.Fatalf("Text: got %q, want %q", resp.Text, "recovered")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("call count: got %d, want 3", got)
	}
}

// A transient error that does not resolve within the retry limit is surfaced to the caller.
func TestOllamaGenerator_Generate_RetriesExhausted(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()),
		llm.WithBackoff(time.Millisecond))
	_, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
		return
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("call count: got %d, want 3 (3 attempts total)", got)
	}
}

// Client-side HTTP 4xx failures are not retried.
func TestOllamaGenerator_Generate_NoRetryOn4xx(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()),
		llm.WithBackoff(time.Millisecond))
	_, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error for 4xx status")
		return
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("call count: got %d, want 1 (4xx must not retry)", got)
	}
}

func TestOllamaGenerator_Generate_NonOKStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()), llm.WithBackoff(time.Millisecond))
	_, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
		return
	}
}

func TestOllamaGenerator_Generate_ContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	_, err := gen.Generate(ctx, ports.GenerateRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
		return
	}
}

// A cancelled context must abort immediately and not trigger retries.
func TestOllamaGenerator_Generate_ContextCancelNoRetry(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()), llm.WithBackoff(time.Millisecond))
	_, err := gen.Generate(ctx, ports.GenerateRequest{Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
		return
	}
	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("call count: got %d, want <= 1 (cancellation must not retry)", got)
	}
}

func TestNewOllamaGenerator_Defaults(t *testing.T) {
	t.Parallel()
	gen := llm.NewOllamaGenerator("")
	if gen == nil {
		t.Fatal("expected non-nil generator")
		return
	}
}

// Option arguments modify the generator consistently regardless of their registration order.
func TestNewOllamaGenerator_OptionOrderIndependence(t *testing.T) {
	t.Parallel()

	const wantText = "order-independent"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"response": wantText})
	}))
	defer srv.Close()

	forward := llm.NewOllamaGenerator("llama3",
		llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	reverse := llm.NewOllamaGenerator("llama3",
		llm.WithHTTPClient(srv.Client()), llm.WithBaseURL(srv.URL))
	for name, gen := range map[string]*llm.OllamaGenerator{"forward": forward, "reverse": reverse} {
		resp, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if resp.Text != wantText {
			t.Fatalf("%s: Text: got %q, want %q", name, resp.Text, wantText)
		}
	}

	def := llm.NewOllamaGenerator("llama3",
		llm.WithBaseURL(""), llm.WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := def.Generate(ctx, ports.GenerateRequest{Prompt: "hi"}); err == nil {
		t.Fatal("expected error: WithBaseURL(\"\") should keep the default base, not srv")
	}
}

func TestOllamaGenerator_Generate_PerCallTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()),
		llm.WithTimeout(50*time.Millisecond), llm.WithBackoff(time.Millisecond))
	start := time.Now()
	_, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected timeout error")
		return
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Generate took %v; per-call timeout not honored", elapsed)
	}
}

func TestOllamaGenerator_Generate_Usage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response":          "hello",
			"prompt_eval_count": 42,
			"eval_count":        17,
		})
	}))
	defer srv.Close()

	gen := llm.NewOllamaGenerator("llama3", llm.WithBaseURL(srv.URL), llm.WithHTTPClient(srv.Client()))
	resp, err := gen.Generate(context.Background(), ports.GenerateRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Usage.PromptTokens != 42 {
		t.Errorf("PromptTokens: got %d, want 42", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 17 {
		t.Errorf("CompletionTokens: got %d, want 17", resp.Usage.CompletionTokens)
	}
	if resp.Usage.Total() != 59 {
		t.Errorf("Total: got %d, want 59", resp.Usage.Total())
	}
}

