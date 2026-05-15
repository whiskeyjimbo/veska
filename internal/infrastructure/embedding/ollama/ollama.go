// Package ollama provides a ports.EmbeddingProvider implementation backed by a
// locally running Ollama instance via POST /api/embeddings.
//
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
	defaultBaseURL = "http://localhost:11434"
	defaultModel   = "nomic-embed-text"
	defaultTimeout = 30 * time.Second
)

// Provider implements ports.EmbeddingProvider against Ollama's /api/embeddings.
// Provider is safe for concurrent use.
type Provider struct {
	baseURL string
	model   string
	client  *http.Client
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

// WithHTTPClient supplies a custom *http.Client. The client's Timeout, if any,
// applies to the entire request; pass an http.Client{} with Timeout: 0 to rely
// solely on context deadlines.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.client = c
		}
	}
}

// WithTimeout sets the per-request timeout on the default http.Client. Ignored
// when WithHTTPClient is also supplied — set the timeout on the custom client
// instead.
func WithTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 && p.client != nil {
			p.client.Timeout = d
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
		baseURL: defaultBaseURL,
		model:   model,
		client:  &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(p)
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

// isNetworkErr classifies wire-level failures (net.OpError, EOF, URL transport
// errors) as unreachable so the search service falls back to lexical mode.
func isNetworkErr(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return false
}
