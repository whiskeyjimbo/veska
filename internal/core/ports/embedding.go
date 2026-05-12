package ports

import "context"

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
