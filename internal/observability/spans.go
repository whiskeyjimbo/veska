package observability

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerProvider is satisfied by both *sdktrace.TracerProvider and
// noop.NewTracerProvider(), allowing callers to use a no-op when
// tracing is disabled.
type TracerProvider interface {
	Tracer(instrumentationName string, opts ...trace.TracerOption) trace.Tracer
}

// StartSpan starts a new span with name using the given TracerProvider.
// The instrumentation library name is "github.com/whiskeyjimbo/veska".
// The caller must call span.End() when the operation completes.
//
// When tp is nil, a noop span is returned so production code never panics.
func StartSpan(ctx context.Context, tp TracerProvider, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if tp == nil {
		return noop.NewTracerProvider().Tracer("").Start(ctx, name, opts...)
	}
	return tp.Tracer("github.com/whiskeyjimbo/veska").Start(ctx, name, opts...)
}
