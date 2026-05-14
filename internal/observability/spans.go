package observability

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// TracerProvider is satisfied by both *sdktrace.TracerProvider and
// trace.NewNoopTracerProvider(), allowing callers to use a no-op when
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
		return trace.NewNoopTracerProvider().Tracer("").Start(ctx, name, opts...)
	}
	return tp.Tracer("github.com/whiskeyjimbo/veska").Start(ctx, name, opts...)
}
