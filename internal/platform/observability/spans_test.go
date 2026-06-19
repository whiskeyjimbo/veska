// SPDX-License-Identifier: AGPL-3.0-only

package observability_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

func TestStartSpan_WithNoopProvider(t *testing.T) {
	np := noop.NewTracerProvider()
	ctx, span := observability.StartSpan(context.Background(), np, "promotion.transaction")
	if ctx == nil {
		t.Error("ctx is nil")
	}
	if span == nil {
		t.Error("span is nil")
	}
	span.End()
}

func TestStartSpan_RecordsSpanName(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := observability.StartSpan(context.Background(), tp, "promotion.transaction")
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "promotion.transaction" {
		t.Errorf("span name: got %q, want %q", spans[0].Name, "promotion.transaction")
	}
}

func TestStartSpan_FourSpanNames(t *testing.T) {
	names := []string{
		"promotion.transaction",
		"mcp.eng_find_symbol",
		"embed.run",
		"parse.file",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithSyncer(exp),
				sdktrace.WithSampler(sdktrace.AlwaysSample()),
			)
			defer func() { _ = tp.Shutdown(context.Background()) }()

			_, span := observability.StartSpan(context.Background(), tp, name)
			span.End()

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("expected 1 span, got %d", len(spans))
			}
			if spans[0].Name != name {
				t.Errorf("span name: got %q, want %q", spans[0].Name, name)
			}
		})
	}
}
