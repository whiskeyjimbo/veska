// Package ollama provides a ports.EmbeddingProvider implementation backed by a
// locally running Ollama instance via POST /api/embeddings.
// Errors caused by an unreachable embedder (dial failures, DNS, EOF mid-flight,
// 5xx responses) are wrapped with ports.ErrEmbedderUnreachable so the
// application-layer search service can fall back to lexical search. 4xx
// responses are caller faults and propagate without the sentinel.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	// DefaultBaseURL is the canonical Ollama base URL used when none is
	// supplied. It is the shared default referenced by wiring layers.
	DefaultBaseURL = "http://localhost:11434"
	// DefaultModel is the canonical embedding model name used when none is
	// supplied. It is the shared default referenced by wiring layers.
	DefaultModel   = "nomic-embed-text"
	defaultTimeout = 30 * time.Second
)

// Provider implements ports.EmbeddingProvider against Ollama's /api/embeddings.
// Provider is safe for concurrent use.
type Provider struct {
	baseURL string
	model   string
	client  *http.Client

	// customClient holds a caller-supplied client (WithHTTPClient). timeout
	// records WithTimeout's intent. Both are recorded during the option loop
	// and reconciled once in New, so the options are order-independent.
	customClient *http.Client
	timeout      time.Duration
}

var _ ports.EmbeddingProvider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithBaseURL overrides the Ollama base URL (default http://localhost:11434).
func WithBaseURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.baseURL = u
		}
	}
}

// ErrMissingDependency is returned by New when the model name is empty. It is
// errors.Is-matchable so callers can distinguish a wiring fault from a runtime
// failure.
var ErrMissingDependency = errors.New("ollama: missing required dependency")

// WithModel overrides the embedding model name (default nomic-embed-text).
// An empty value is ignored.
func WithModel(m string) Option {
	return func(p *Provider) {
		if m != "" {
			p.model = m
		}
	}
}

// WithHTTPClient supplies a caller-owned *http.Client, used as-is and never
// mutated. The client's Timeout, if any, applies to the entire request; pass an
// http.Client{} with Timeout: 0 to rely solely on context deadlines. When
// supplied this wins: WithTimeout is ignored regardless of option order.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.customClient = c
		}
	}
}

// WithTimeout sets the per-request timeout on the default http.Client. Ignored
// when WithHTTPClient is also supplied — set the timeout on the custom client
// instead. The effect is order-independent: it never mutates a caller-supplied
// client.
func WithTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.timeout = d
		}
	}
}

// New constructs a Provider. model must be non-empty; an empty model yields an
// error wrapping ErrMissingDependency and a nil *Provider. Apply WithBaseURL /
// WithModel / WithHTTPClient / WithTimeout to override defaults.
func New(model string, opts ...Option) (*Provider, error) {
	if model == "" {
		return nil, fmt.Errorf("ollama.New: model must not be empty: %w", ErrMissingDependency)
	}
	p := &Provider{
		baseURL: DefaultBaseURL,
		model:   model,
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(p)
	}
	// Build the client once, after all options are recorded, so the result is
	// independent of option order. A caller-supplied client wins and is used
	// unchanged; otherwise the default client adopts the configured timeout.
	if p.customClient != nil {
		p.client = p.customClient
	} else {
		p.client = &http.Client{Timeout: p.timeout}
	}
	return p, nil
}

// ModelID returns the configured embedding model name.
func (p *Provider) ModelID() string { return p.model }

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed sends text to Ollama and returns the resulting embedding vector.
// Network-class failures and 5xx responses are wrapped with
// ports.ErrEmbedderUnreachable; 4xx responses propagate as plain errors.
func (p *Provider) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: p.model, Prompt: text})
	if err != nil {
		return nil, fmt.Errorf("ollama embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		// Honor caller-driven cancellation without triggering fallback.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ollama embed: %w", ctxErr)
		}
		return nil, fmt.Errorf("ollama embed: %w: %w", ports.ErrEmbedderUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("ollama embed: status %d: %w", resp.StatusCode, ports.ErrEmbedderUnreachable)
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama embed: status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// Mid-flight EOF / connection drop counts as unreachable.
		if isNetworkErr(err) {
			return nil, fmt.Errorf("ollama embed: decode: %w: %w", ports.ErrEmbedderUnreachable, err)
		}
		return nil, fmt.Errorf("ollama embed: decode response: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("ollama embed: empty embedding in response")
	}
	return out.Embedding, nil
}

type batchRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type batchResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// EmbedBatch sends multiple texts to Ollama's POST /api/embed in a
// single request and returns embeddings in the same order. One network
// roundtrip instead of N — primary lever for 's cobra
// cold-scan dropping from ~15s to <3s. Empty input is a no-op.
// Failure modes mirror Embed: 5xx / net errors are wrapped with
// ErrEmbedderUnreachable so callers can fall back; 4xx propagates as
// a plain error. A short response (fewer embeddings than texts) is
// treated as malformed.
func (p *Provider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(batchRequest{Model: p.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("ollama embed_batch: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed_batch: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ollama embed_batch: %w", ctxErr)
		}
		return nil, fmt.Errorf("ollama embed_batch: %w: %w", ports.ErrEmbedderUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("ollama embed_batch: status %d: %w", resp.StatusCode, ports.ErrEmbedderUnreachable)
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama embed_batch: status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var out batchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if isNetworkErr(err) {
			return nil, fmt.Errorf("ollama embed_batch: decode: %w: %w", ports.ErrEmbedderUnreachable, err)
		}
		return nil, fmt.Errorf("ollama embed_batch: decode response: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed_batch: expected %d embeddings, got %d", len(texts), len(out.Embeddings))
	}
	return out.Embeddings, nil
}

// isNetworkErr classifies wire-level failures (net.OpError, EOF, URL transport
// errors) as unreachable so the search service falls back to lexical mode.
func isNetworkErr(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	_, ok := errors.AsType[*url.Error](err)
	return ok
}
