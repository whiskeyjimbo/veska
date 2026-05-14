package ports

import (
	"context"
	"errors"
)

// ErrEmbedderUnreachable is the sentinel returned by EmbeddingProvider.Embed
// when the underlying embedder is unreachable (e.g. Ollama is down or the
// HTTP dial fails). It is the ONLY embedder error that triggers lexical
// fallback in the application-layer search service (m3.03.2); any other
// error propagates wrapped to the caller. Adapters must use errors.Is /
// fmt.Errorf("...: %w", ErrEmbedderUnreachable) to participate.
var ErrEmbedderUnreachable = errors.New("embedder unreachable")

// EmbeddingProvider is the port for generating vector embeddings from text.
// Implementations are provided by infrastructure adapters (e.g. Ollama, OpenAI).
type EmbeddingProvider interface {
	// Embed returns a vector embedding for the given text. The dimensionality of
	// the returned slice is determined by the underlying model and is stable for
	// the lifetime of a single ModelID value.
	Embed(ctx context.Context, text string) ([]float32, error)

	// ModelID returns the stable identifier of the embedding model in use
	// (e.g. "nomic-embed-text"). Callers may use this value as a cache key or
	// to detect model changes that require reindexing.
	ModelID() string
}
