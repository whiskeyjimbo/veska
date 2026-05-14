package application

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// Ingester orchestrates the parse-on-save hot path:
// CodeParser.ParseFile → StagingArea write.
// It never touches SQLite for graph writes; all graph state goes to the
// in-memory StagingArea only. Parse failures, however, are surfaced as
// Findings through the optional FindingStorage port (set via
// SetFindingStorage) so structural problems are visible to operators before
// commit-seal time.
type Ingester struct {
	parser   ports.CodeParser
	staging  *StagingArea
	gate     *IngestionGate
	tp       observability.TracerProvider
	findings atomic.Pointer[ports.FindingStorage]
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

// SetFindingStorage installs the sink for parse-failure findings emitted at
// ingest time. Calling with a nil storage clears the sink (no findings will
// be emitted). The setter is safe for concurrent use.
func (ing *Ingester) SetFindingStorage(s ports.FindingStorage) {
	if s == nil {
		ing.findings.Store(nil)
		return
	}
	ing.findings.Store(&s)
}

// findingStorage returns the currently installed FindingStorage, or nil if none.
func (ing *Ingester) findingStorage() ports.FindingStorage {
	p := ing.findings.Load()
	if p == nil {
		return nil
	}
	return *p
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
	ing.emitParseFailures(ctx, repoID, branch, path, result.Failures)
	return nil
}

// emitParseFailures forwards each ParseFailure to the configured FindingStorage
// as a single 'parse-failure' Finding anchored to the file. Emission is
// best-effort: a nil sink is a no-op (call site does not require a sink) and
// per-call errors are logged but not propagated — the Save hot path must not
// fail because a finding could not be persisted.
//
// All failures for one (repo, branch, file) collapse to one finding_id because
// domain.NewFinding hashes (rule, anchor) only; the SQLite repo's
// ON CONFLICT(finding_id, branch) clause then guarantees at most one OPEN row.
func (ing *Ingester) emitParseFailures(ctx context.Context, repoID, branch, path string, failures []domain.ParseFailure) {
	if len(failures) == 0 {
		return
	}
	sink := ing.findingStorage()
	if sink == nil {
		return
	}

	// Use the first (most actionable) failure for the message body. Tree-sitter
	// tends to cascade syntax errors, so surfacing only the first keeps the
	// finding stable and avoids noise.
	first := failures[0]
	msg := fmt.Sprintf("parse failure in %s", path)
	if first.Message != "" {
		if first.Line > 0 {
			msg = fmt.Sprintf("parse failure in %s at line %d: %s", path, first.Line, first.Message)
		} else {
			msg = fmt.Sprintf("parse failure in %s: %s", path, first.Message)
		}
	}

	f, err := domain.NewFinding(
		"", // ID: per-row PK is assigned by the storage layer; FindingID (branch-stable) is what matters here.
		repoID, branch,
		domain.SeverityMedium,
		domain.LayerStructural,
		"parse-failure",
		msg,
		domain.WithFileAnchor(path),
	)
	if err != nil {
		slog.Warn("ingester: failed to construct parse-failure finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
		return
	}
	if err := sink.Save(ctx, f); err != nil {
		slog.Warn("ingester: failed to save parse-failure finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
	}
}

// DeleteFile removes the file from staging. Called when a file is deleted on disk.
func (ing *Ingester) DeleteFile(repoID, branch, path string) {
	ing.staging.DeleteStagedFile(repoID, branch, path)
}
