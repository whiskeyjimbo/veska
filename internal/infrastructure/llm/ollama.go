// SPDX-License-Identifier: AGPL-3.0-only

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
	defaultOllamaBase  = "http://localhost:11434"
	defaultOllamaModel = "llama3"

	// maxAttempts specifies the maximum number of request attempts before giving up.
	maxAttempts = 3

	// defaultBackoff is the initial delay before retrying a failed request using exponential backoff.
	defaultBackoff = 500 * time.Millisecond

	// defaultTimeout bounds a single Generate call, encompassing all retry attempts.
	defaultTimeout = 60 * time.Second
)

// ollamaGenerateRequest defines the JSON payload sent to the Ollama /api/generate endpoint.
// Format is Ollama's structured-output schema that constrains model output; it is omitted
// when empty so plain-text generation is unaffected.
type ollamaGenerateRequest struct {
	Model      string          `json:"model"`
	Prompt     string          `json:"prompt"`
	NumPredict int             `json:"num_predict,omitempty"`
	Stream     bool            `json:"stream"`
	Format     json.RawMessage `json:"format,omitempty"`
}

// ollamaGenerateResponse represents the JSON response from the Ollama /api/generate endpoint.
// PromptEvalCount and EvalCount represent token usage stats reported by Ollama.
type ollamaGenerateResponse struct {
	Response        string `json:"response"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// OllamaGenerator generates text completions using a local or remote Ollama service.
// Transient HTTP 5xx errors and network failures are retried with exponential backoff,
// while client-side HTTP 4xx failures and context cancellations abort immediately.
// OllamaGenerator is safe for concurrent use.
type OllamaGenerator struct {
	baseURL string
	model   string
	client  *http.Client
	backoff time.Duration
	timeout time.Duration

	// customClient holds a caller-supplied HTTP client to ensure option order independence.
	customClient *http.Client
}

var _ ports.LLMGenerator = (*OllamaGenerator)(nil)

type Option func(*OllamaGenerator)

// WithBaseURL configures a custom Ollama service base URL.
func WithBaseURL(u string) Option {
	return func(g *OllamaGenerator) {
		if u != "" {
			g.baseURL = u
		}
	}
}

// WithHTTPClient registers a custom HTTP client for Ollama API requests.
func WithHTTPClient(c *http.Client) Option {
	return func(g *OllamaGenerator) {
		if c != nil {
			g.customClient = c
		}
	}
}

// WithBackoff configures the initial delay duration before the first retry.
func WithBackoff(d time.Duration) Option {
	return func(g *OllamaGenerator) {
		if d > 0 {
			g.backoff = d
		}
	}
}

// WithTimeout bounds the lifetime of a complete Generate call, including retries.
func WithTimeout(d time.Duration) Option {
	return func(g *OllamaGenerator) {
		if d > 0 {
			g.timeout = d
		}
	}
}

// NewOllamaGenerator creates a generator targeting the specified model.
func NewOllamaGenerator(model string, opts ...Option) *OllamaGenerator {
	if model == "" {
		model = defaultOllamaModel
	}
	g := &OllamaGenerator{
		baseURL: defaultOllamaBase,
		model:   model,
		backoff: defaultBackoff,
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(g)
	}

	if g.customClient != nil {
		g.client = g.customClient
	} else {
		g.client = http.DefaultClient
	}
	return g
}

// retryableError wraps an underlying error to signal that the failed operation can be retried.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// Generate requests a text completion from Ollama.
func (g *OllamaGenerator) Generate(ctx context.Context, req ports.GenerateRequest) (ports.GenerateResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	body := ollamaGenerateRequest{
		Model:      g.model,
		Prompt:     req.Prompt,
		NumPredict: req.MaxTokens,
		Stream:     false,
		Format:     req.Format,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, genErr := g.doGenerate(ctx, encoded)
		if genErr == nil {
			return ports.GenerateResponse{
				Text:       out.Response,
				Provenance: g.provenance(req),
				Usage: ports.TokenUsage{
					PromptTokens:     out.PromptEvalCount,
					CompletionTokens: out.EvalCount,
				},
			}, nil
		}
		lastErr = genErr

		if ctx.Err() != nil {
			return ports.GenerateResponse{}, genErr
		}

		var retry *retryableError
		if !errors.As(genErr, &retry) {
			return ports.GenerateResponse{}, genErr
		}
		if attempt == maxAttempts {
			break
		}

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

// doGenerate executes a single HTTP request to the Ollama endpoint.
// It wraps transient issues in a retryableError so the retry loop can identify them.
func (g *OllamaGenerator) doGenerate(ctx context.Context, encoded []byte) (ollamaGenerateResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/api/generate", bytes.NewReader(encoded))
	if err != nil {
		return ollamaGenerateResponse{}, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ollamaGenerateResponse{}, &retryableError{fmt.Errorf("ollama: POST /api/generate: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		statusErr := fmt.Errorf("ollama: POST /api/generate: status %d", resp.StatusCode)
		if resp.StatusCode >= 500 {
			return ollamaGenerateResponse{}, &retryableError{statusErr}
		}
		return ollamaGenerateResponse{}, statusErr
	}

	var out ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ollamaGenerateResponse{}, fmt.Errorf("ollama: decode response: %w", err)
	}
	return out, nil
}

func (g *OllamaGenerator) provenance(req ports.GenerateRequest) ports.Provenance {
	sum := sha256.Sum256([]byte(req.Prompt))
	return ports.Provenance{
		ModelID:               g.model,
		PromptTemplateVersion: req.PromptTemplateVersion,
		InputHash:             hex.EncodeToString(sum[:]),
	}
}
