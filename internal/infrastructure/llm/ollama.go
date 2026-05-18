// Package llm provides LLMGenerator implementations for the veska module.
package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	// defaultOllamaBase is the default Ollama API base URL.
	defaultOllamaBase = "http://localhost:11434"

	// defaultOllamaModel is the default text-generation model used when none
	// is specified.
	defaultOllamaModel = "llama3"

	// maxAttempts is the total number of Generate attempts (1 initial + 2
	// retries) made before a transient failure is surfaced to the caller.
	maxAttempts = 3

	// defaultBackoff is the base delay before the first retry. Each subsequent
	// retry doubles it (exponential backoff).
	defaultBackoff = 500 * time.Millisecond

	// defaultTimeout bounds a single Generate call (across all attempts) when
	// no WithTimeout option is supplied.
	defaultTimeout = 60 * time.Second
)

// ollamaGenerateRequest is the JSON body for POST /api/generate.
type ollamaGenerateRequest struct {
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	NumPredict int    `json:"num_predict,omitempty"`
	Stream     bool   `json:"stream"`
}

// ollamaGenerateResponse is the JSON body returned by POST /api/generate
// when stream is false.
type ollamaGenerateResponse struct {
	Response string `json:"response"`
}

// OllamaGenerator is an LLMGenerator backed by a locally running Ollama
// instance. It POSTs to /api/generate with stream:false and returns the
// complete generated text in a single call.
//
// Transient failures (HTTP 5xx or network/transport errors) are retried up to
// maxAttempts times with exponential backoff; HTTP 4xx responses and context
// cancellation are surfaced immediately without retry.
//
// OllamaGenerator is safe for concurrent use.
type OllamaGenerator struct {
	baseURL string
	model   string
	client  *http.Client
	backoff time.Duration
	timeout time.Duration
}

// Compile-time interface satisfaction check.
var _ ports.LLMGenerator = (*OllamaGenerator)(nil)

// Option customises an OllamaGenerator at construction time.
type Option func(*OllamaGenerator)

// WithBackoff sets the base delay before the first retry. Each subsequent
// retry doubles it. A non-positive value is ignored.
func WithBackoff(d time.Duration) Option {
	return func(g *OllamaGenerator) {
		if d > 0 {
			g.backoff = d
		}
	}
}

// WithTimeout bounds a single Generate call — including all retry attempts —
// to d. A non-positive value is ignored.
func WithTimeout(d time.Duration) Option {
	return func(g *OllamaGenerator) {
		if d > 0 {
			g.timeout = d
		}
	}
}

// NewOllamaGenerator constructs an OllamaGenerator with the given base URL and
// model. Pass empty strings to use the defaults (http://localhost:11434 and
// "llama3"). The provided http.Client is used for all requests; pass nil to use
// http.DefaultClient. Optional behaviour (backoff, per-call timeout) is set via
// Option values.
func NewOllamaGenerator(baseURL, model string, client *http.Client, opts ...Option) *OllamaGenerator {
	if baseURL == "" {
		baseURL = defaultOllamaBase
	}
	if model == "" {
		model = defaultOllamaModel
	}
	if client == nil {
		client = http.DefaultClient
	}
	g := &OllamaGenerator{
		baseURL: baseURL,
		model:   model,
		client:  client,
		backoff: defaultBackoff,
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// retryableError marks an HTTP 5xx response so the retry loop can distinguish
// it from a non-retryable 4xx.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Generate sends req to the Ollama /api/generate endpoint and returns the
// generated text together with its Provenance. Transient failures are retried
// with exponential backoff; the call is bounded by the configured per-call
// timeout and respects ctx cancellation.
func (g *OllamaGenerator) Generate(ctx context.Context, req ports.GenerateRequest) (ports.GenerateResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	body := ollamaGenerateRequest{
		Model:      g.model,
		Prompt:     req.Prompt,
		NumPredict: req.MaxTokens,
		Stream:     false,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		text, genErr := g.doGenerate(ctx, encoded)
		if genErr == nil {
			return ports.GenerateResponse{
				Text:       text,
				Provenance: g.provenance(req),
			}, nil
		}
		lastErr = genErr

		// Context cancellation/deadline is terminal — never retry.
		if ctx.Err() != nil {
			return ports.GenerateResponse{}, genErr
		}
		// Only HTTP 5xx and transport errors are retryable; a 4xx is not.
		var retry *retryableError
		if !errors.As(genErr, &retry) {
			return ports.GenerateResponse{}, genErr
		}
		if attempt == maxAttempts {
			break
		}

		// Exponential backoff, interruptible by ctx.
		delay := g.backoff << (attempt - 1)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ports.GenerateResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return ports.GenerateResponse{}, fmt.Errorf("ollama: generate failed after %d attempts: %w", maxAttempts, lastErr)
}

// doGenerate performs a single POST /api/generate. It returns a *retryableError
// for HTTP 5xx and transport-level failures so the caller can decide to retry.
func (g *OllamaGenerator) doGenerate(ctx context.Context, encoded []byte) (string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/api/generate", bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		// Transport errors (DNS, connection refused, reset) are transient.
		// A cancelled/expired context surfaces here too; the retry loop
		// checks ctx.Err() and will not retry it despite the wrapper.
		return "", &retryableError{fmt.Errorf("ollama: POST /api/generate: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		statusErr := fmt.Errorf("ollama: POST /api/generate: status %d", resp.StatusCode)
		if resp.StatusCode >= 500 {
			return "", &retryableError{statusErr}
		}
		return "", statusErr
	}

	var out ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ollama: decode response: %w", err)
	}
	return out.Response, nil
}

// provenance builds the Provenance for a successful call: the generator's
// model, the request's prompt-template version echoed back, and a sha256 hex
// digest of the request prompt.
func (g *OllamaGenerator) provenance(req ports.GenerateRequest) ports.Provenance {
	sum := sha256.Sum256([]byte(req.Prompt))
	return ports.Provenance{
		ModelID:               g.model,
		PromptTemplateVersion: req.PromptTemplateVersion,
		InputHash:             hex.EncodeToString(sum[:]),
	}
}
