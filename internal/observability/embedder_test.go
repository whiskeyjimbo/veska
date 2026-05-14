package observability_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/whiskeyjimbo/veska/internal/observability"
)

// stubEmbedder is a minimal EmbeddingProvider for testing.
type stubEmbedder struct{}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

func (s *stubEmbedder) ModelID() string { return "stub-model" }

func TestInstrumentedEmbedder_EmitsEmbedRunSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	inner := &stubEmbedder{}
	instrumented := observability.NewInstrumentedEmbedder(inner, tp)

	_, err := instrumented.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "embed.run" {
		t.Errorf("span name: got %q, want %q", spans[0].Name, "embed.run")
	}
}

func TestInstrumentedEmbedder_NilProvider(t *testing.T) {
	inner := &stubEmbedder{}
	instrumented := observability.NewInstrumentedEmbedder(inner, nil)

	vec, err := instrumented.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) == 0 {
		t.Error("expected non-empty embedding")
	}
}
