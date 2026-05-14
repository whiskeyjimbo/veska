package ports

// Embedder is a type alias for EmbeddingProvider.
//
// EmbeddingProvider already declares the Embed(ctx, text) ([]float32, error)
// method and the ModelID() string accessor that callers need. Embedder is
// provided as a convenience alias so application-layer code can refer to the
// concept by its shorter name without duplicating the interface definition.
//
// Use EmbeddingProvider when you also need ModelID(); use Embedder when the
// call site only cares about the embedding operation.
type Embedder = EmbeddingProvider
