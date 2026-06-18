package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// NewTracerProvider constructs an OTLP gRPC TracerProvider with a
// parentbased_traceidratio sampler at sampleRatio.
// endpoint must be a non-empty host:port string (e.g. "localhost:4317").
// Returns an error when endpoint is empty - the caller must check both
// tracing.enabled=true AND VESKA_OTLP_ENDPOINT before calling this.
// sampleRatio is the head-sampling probability applied to root spans
// (0.0 drops all, 1.0 keeps all); the caller is responsible for bounding
// it to [0.0, 1.0] (config.Config.Validate enforces this at startup).
// The OTLP exporter dials lazily; construction does not fail when the
// collector is unreachable.
func NewTracerProvider(endpoint string, sampleRatio float64) (*sdktrace.TracerProvider, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("observability: OTLP endpoint must not be empty; set VESKA_OTLP_ENDPOINT")
	}

	exp, err := otlptracegrpc.New(
		context.Background(),
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: create OTLP exporter: %w", err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sampler),
	)
	return tp, nil
}
