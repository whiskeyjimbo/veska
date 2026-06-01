package observability

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// InstrumentedEmbedder wraps a ports.EmbeddingProvider and emits an
// "embed.run" span around every call to Embed.
//
// When tp is nil a noop span is used so production code never panics.
type InstrumentedEmbedder struct {
	inner ports.EmbeddingProvider
	tp    TracerProvider
}

// NewInstrumentedEmbedder wraps inner with span instrumentation.
// Pass a nil tp to use a noop provider (no-op when tracing is disabled).
func NewInstrumentedEmbedder(inner ports.EmbeddingProvider, tp TracerProvider) *InstrumentedEmbedder {
	return &InstrumentedEmbedder{inner: inner, tp: tp}
}

// Embed emits an "embed.run" span and delegates to the inner provider.
func (e *InstrumentedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	ctx, span := StartSpan(ctx, e.tp, "embed.run")
	defer span.End()
	return e.inner.Embed(ctx, text)
}

// ModelID delegates to the inner provider.
func (e *InstrumentedEmbedder) ModelID() string {
	return e.inner.ModelID()
}

// EmbedBatch passes through to the inner provider's BatchEmbeddingProvider
// implementation if it has one — preserves the batch fast path through
// the tracing wrapper . Without this, embedder.Worker's
// type assertion on the wrapped provider fails and we degrade to N
// serial Embed calls. Span is named "embed.batch" with the batch size
// as an attribute (TODO once metrics needs it).
func (e *InstrumentedEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	bp, ok := e.inner.(ports.BatchEmbeddingProvider)
	if !ok {
		return nil, ports.ErrBatchEmbedNotSupported
	}
	ctx, span := StartSpan(ctx, e.tp, "embed.batch")
	defer span.End()
	return bp.EmbedBatch(ctx, texts)
}

// Compile-time check: InstrumentedEmbedder satisfies ports.EmbeddingProvider
// AND ports.BatchEmbeddingProvider (the latter degrades when the wrapped
// provider doesn't implement it; see EmbedBatch).
var (
	_ ports.EmbeddingProvider      = (*InstrumentedEmbedder)(nil)
	_ ports.BatchEmbeddingProvider = (*InstrumentedEmbedder)(nil)
)
