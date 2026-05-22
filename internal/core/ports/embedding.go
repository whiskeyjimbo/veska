package ports

import (
	"context"
	"errors"
)

// ErrBatchEmbedNotSupported is the sentinel returned by a
// BatchEmbeddingProvider whose underlying provider does not actually
// support batch embedding. Wrappers (e.g. instrumentation) that
// implement the batch interface but delegate to a non-batch inner use
// this to signal "fall back to per-text Embed". Callers should not
// retry on this error.
var ErrBatchEmbedNotSupported = errors.New("embedder: batch not supported")

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

// BatchEmbeddingProvider is the optional batch interface — adapters that
// can embed many texts in a single network roundtrip implement this in
// addition to EmbeddingProvider, and the embedder Worker prefers it
// (solov2-ucp). Implementations MUST preserve input order and return
// exactly len(texts) vectors; partial successes return ErrEmbedderUnreachable
// (or another wrapped error) for the whole batch — the worker retries
// individually on failure.
//
// Empty input returns (nil, nil). A nil or zero-length individual text
// in the slice is the caller's responsibility to filter out; adapters
// may pass it through to the underlying service.
type BatchEmbeddingProvider interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}
