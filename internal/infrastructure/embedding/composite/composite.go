// Package composite wires two EmbeddingProviders into a primary →
// secondary fallback chain (solov2-soc). Used at the daemon
// composition root to layer the in-process static embedder behind
// Ollama: a fresh machine without Ollama still gets working search,
// and a machine with Ollama running gets the higher-quality vectors.
//
// Fallback is gated on ports.ErrEmbedderUnreachable — every other
// primary error propagates wrapped so a "model not found" or a 5xx
// from a misconfigured backend doesn't silently mask itself.
package composite

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by New when either provider is nil.
var ErrMissingDependency = errors.New("composite: missing required dependency")

// Provider is the composite EmbeddingProvider adapter.
type Provider struct {
	primary   ports.EmbeddingProvider
	secondary ports.EmbeddingProvider
	modelID   string
}

// New constructs a Provider. Both primary and secondary are required:
// a nil dependency yields an error wrapping ErrMissingDependency and
// a nil *Provider — wiring faults surface at construction, not at the
// first Embed call. The composite's ModelID combines both inner IDs
// so swapping either side invalidates the embedding cache.
func New(primary, secondary ports.EmbeddingProvider) (*Provider, error) {
	if primary == nil {
		return nil, fmt.Errorf("composite.New: primary is nil: %w", ErrMissingDependency)
	}
	if secondary == nil {
		return nil, fmt.Errorf("composite.New: secondary is nil: %w", ErrMissingDependency)
	}
	return &Provider{
		primary:   primary,
		secondary: secondary,
		modelID:   "composite(" + primary.ModelID() + "->" + secondary.ModelID() + ")",
	}, nil
}

// Embed tries primary first; on ports.ErrEmbedderUnreachable it
// retries through secondary. Any other primary error propagates as-is
// — a malformed-input or model-not-found is not a fallback condition.
//
// When both providers report ErrEmbedderUnreachable the secondary's
// error surfaces so callers (search.Service) can still route to the
// lexical fallback. The sentinel must remain wrapped end-to-end.
func (p *Provider) Embed(ctx context.Context, text string) ([]float32, error) {
	vec, err := p.primary.Embed(ctx, text)
	if err == nil {
		return vec, nil
	}
	if !errors.Is(err, ports.ErrEmbedderUnreachable) {
		return nil, fmt.Errorf("composite: primary embed: %w", err)
	}
	vec, err = p.secondary.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("composite: secondary embed: %w", err)
	}
	return vec, nil
}

// ModelID returns the composite identifier — the primary's and
// secondary's IDs combined, so a config swap invalidates the cache.
func (p *Provider) ModelID() string { return p.modelID }
