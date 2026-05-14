package application

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// Ingester orchestrates the parse-on-save hot path:
// CodeParser.ParseFile → StagingArea write.
// It never touches SQLite; all writes go to the in-memory StagingArea only.
type Ingester struct {
	parser  ports.CodeParser
	staging *StagingArea
	gate    *IngestionGate
	tp      observability.TracerProvider
}

// NewIngester constructs an Ingester wired to the provided parser, staging area,
// and ingestion gate. The gate guards against branch-switch races.
func NewIngester(parser ports.CodeParser, staging *StagingArea, gate *IngestionGate) *Ingester {
	return &Ingester{
		parser:  parser,
		staging: staging,
		gate:    gate,
	}
}

// SetTracerProvider installs a TracerProvider for parse.file spans.
// If not called (or called with nil), a noop provider is used.
func (ing *Ingester) SetTracerProvider(tp observability.TracerProvider) {
	ing.tp = tp
}

// tracerProvider returns the configured provider or a noop if nil.
func (ing *Ingester) tracerProvider() observability.TracerProvider {
	if ing.tp == nil {
		return trace.NewNoopTracerProvider()
	}
	return ing.tp
}

// Save parses src for the file at path and stages the result.
// repoID and branch scope the staging slot.
// Parse errors are non-fatal: if ParseFile returns an error, the error is logged
// at WARN level and Save returns nil — the file is simply not staged.
// Save does NOT touch SQLite.
func (ing *Ingester) Save(ctx context.Context, repoID, branch, path string, src []byte) error {
	// Block if a branch switch is in progress; read current generation before parsing
	// so the generation check in StageIfCurrentGeneration is tight.
	ing.gate.WaitIfPaused()
	gen := ing.gate.Generation()

	ctx, span := observability.StartSpan(ctx, ing.tracerProvider(), "parse.file")
	defer span.End()

	result, err := ing.parser.ParseFile(ctx, repoID, path, src)
	if err != nil {
		slog.Warn("ingester: parse error; file not staged",
			"repoID", repoID,
			"branch", branch,
			"path", path,
			"err", err,
		)
		return nil
	}
	ing.staging.StageIfCurrentGeneration(repoID, branch, path, result.Nodes, result.Edges, gen, ing.gate)
	return nil
}

// DeleteFile removes the file from staging. Called when a file is deleted on disk.
func (ing *Ingester) DeleteFile(repoID, branch, path string) {
	ing.staging.DeleteStagedFile(repoID, branch, path)
}
