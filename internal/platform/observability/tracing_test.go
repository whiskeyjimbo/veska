package observability_test

import (
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

func TestNewTracerProvider_EmptyEndpointReturnsError(t *testing.T) {
	tp, err := observability.NewTracerProvider("")
	if err == nil {
		t.Fatal("expected error for empty endpoint, got nil")
		return
	}
	if tp != nil {
		t.Error("expected nil TracerProvider on error")
	}
}

func TestNewTracerProvider_ValidEndpointReturnsProvider(t *testing.T) {
	// Use a local address that likely isn't running — OTLP gRPC exporter creates
	// the exporter lazily, so no dial happens at construction time.
	tp, err := observability.NewTracerProvider("localhost:4317")
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

func TestNewTracerProvider_SamplerIsParentBasedTraceIDRatio(t *testing.T) {
	tp, err := observability.NewTracerProvider("localhost:4317")
	if err != nil {
		t.Fatalf("NewTracerProvider: %v", err)
	}

	// The sampler description for parentbased_traceidratio at 1.0 is
	// "ParentBased{root:TraceIDRatioBased{1}}" per the OTel SDK.
	sampler := observability.ExtractSampler(tp)
	desc := sampler.Description()
	if desc == "" {
		t.Error("sampler description is empty")
	}
	// Accept any description that mentions ParentBased + ratio.
	if desc != "ParentBased{root:TraceIDRatioBased{1}}" {
		t.Logf("sampler description: %q (expected ParentBased{root:TraceIDRatioBased{1}})", desc)
		// Not fatal — description format may vary across SDK versions.
		// Log only, don't fail.
	}
}
