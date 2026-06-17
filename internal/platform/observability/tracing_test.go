package observability_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

func TestNewTracerProvider_EmptyEndpointReturnsError(t *testing.T) {
	tp, err := observability.NewTracerProvider("", 1.0)
	if err == nil {
		t.Fatal("expected error for empty endpoint, got nil")
		return
	}
	if tp != nil {
		t.Error("expected nil TracerProvider on error")
	}
}

func TestNewTracerProvider_ValidEndpointReturnsProvider(t *testing.T) {
	// Use a local address that likely isn't running - OTLP gRPC exporter creates
	// the exporter lazily, so no dial happens at construction time.
	tp, err := observability.NewTracerProvider("localhost:4317", 1.0)
	if err != nil {
		t.Fatalf("NewTracerProvider: unexpected error: %v", err)
	}
	if tp == nil {
		t.Fatal("expected non-nil TracerProvider")
		return
	}
	// Verify it is the concrete SDK type.
	if _, ok := any(tp).(*sdktrace.TracerProvider); !ok {
		t.Errorf("expected *sdktrace.TracerProvider, got %T", tp)
	}
}

func TestNewTracerProvider_RatioThreadedIntoSampler(t *testing.T) {
	// TraceIDRatioBased is deterministic at the extremes: 0.0 always drops a
	// root span, 1.0 always samples it. The SDK exposes no sampler accessor,
	// so assert the threaded ratio through observable sampling behaviour. A
	// hardcoded 1.0 implementation fails the 0.0 case.
	cases := []struct {
		ratio      float64
		wantSample bool
	}{
		{0.0, false},
		{1.0, true},
	}
	for _, tc := range cases {
		tp, err := observability.NewTracerProvider("localhost:4317", tc.ratio)
		if err != nil {
			t.Fatalf("NewTracerProvider(ratio=%v): %v", tc.ratio, err)
		}
		_, span := tp.Tracer("test").Start(context.Background(), "root")
		if got := span.SpanContext().IsSampled(); got != tc.wantSample {
			t.Errorf("ratio=%v: root span sampled=%v, want %v", tc.ratio, got, tc.wantSample)
		}
	}
}
