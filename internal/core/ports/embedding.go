package ports

import (
	"context"
	"errors"
)

// ErrBatchEmbedNotSupported is returned by a BatchEmbeddingProvider whose
// underlying provider does not actually support batch embedding. Wrappers
// that implement the batch interface but delegate to a non-batch inner provider
// use this to signal fallback to per-text Embed. Callers should not retry on this error.
var ErrBatchEmbedNotSupported = errors.New("embedder: batch not supported")

// ErrEmbedderUnreachable is returned by EmbeddingProvider.Embed when the
// underlying embedder is unreachable. It is the only embedder error that triggers
// lexical fallback in the application-layer search service; any other error
// propagates wrapped to the caller. Adapters must wrap this error using %w
// to participate.
var ErrEmbedderUnreachable = errors.New("embedder unreachable")

// EmbeddingProvider is the port for generating vector embeddings from text.
type EmbeddingProvider interface {
	// Dimensionality of the returned slice is determined by the underlying model
	// and is stable for the lifetime of a single ModelID value.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Callers may use this value as a cache key or to detect model changes
	// that require reindexing.
	ModelID() string
}

// BatchEmbeddingProvider is the optional batch interface. The embedder worker
// prefers it if implemented. Implementations must preserve input order and return
// exactly len(texts) vectors. Partial successes must return a wrapped error
// for the whole batch, prompting the worker to retry individually on failure.
// Empty input returns a nil result. Empty or nil individual text elements
// are the caller's responsibility to filter out.
type BatchEmbeddingProvider interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}
