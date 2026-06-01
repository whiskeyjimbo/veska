package application

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// Ingester orchestrates the parse-on-save hot path:
// CodeParser.ParseFile → staging.Area write.
// It never touches SQLite for graph writes; all graph state goes to the
// in-memory staging.Area only. Parse failures, however, are surfaced as
// Findings through the optional FindingStorage port (wired via
// WithFindingStorage) so structural problems are visible to operators before
// commit-seal time.
type Ingester struct {
	parser  ports.CodeParser
	staging *staging.Area
	gate    *staging.Gate
	tp      observability.TracerProvider
	emitter *findingEmitter
}

// IngesterOption configures optional Ingester collaborators at construction.
// The required parser/staging/gate are positional; everything tunable is an
// option so the constructed Ingester is immutable and fully wired before use.
type IngesterOption func(*Ingester)

// WithIngesterTracerProvider installs a TracerProvider for parse.file spans.
// If omitted (or given nil), a noop provider is used.
func WithIngesterTracerProvider(tp observability.TracerProvider) IngesterOption {
	return func(ing *Ingester) { ing.tp = tp }
}

// WithFindingStorage installs the sink for parse-failure / todo findings emitted
// at ingest time. If omitted, no findings are emitted. The sink is wired once at
// construction into the findingEmitter, so the Save hot path reads it without
// synchronisation.
func WithFindingStorage(s ports.FindingStorage) IngesterOption {
	return func(ing *Ingester) { ing.emitter = newFindingEmitter(s) }
}

// NewIngester constructs an Ingester wired to the provided parser, staging area,
// and ingestion gate. The gate guards against branch-switch races. Optional
// collaborators (tracer, finding storage) are supplied via IngesterOption.
func NewIngester(parser ports.CodeParser, area *staging.Area, gate *staging.Gate, opts ...IngesterOption) *Ingester {
	ing := &Ingester{
		parser:  parser,
		staging: area,
		gate:    gate,
	}
	for _, o := range opts {
		o(ing)
	}
	if ing.emitter == nil {
		// No FindingStorage supplied: a no-op emitter keeps the Save path
		// branch-free (its methods short-circuit on a nil sink).
		ing.emitter = newFindingEmitter(nil)
	}
	return ing
}

// SupportedExtensions reports the file extensions the wired parser can read,
// or nil when the parser does not enumerate them. The cold scan reads this to
// filter its walk so the extension set lives with the parser rather than being
// duplicated in coldscan.go (solov2-xde2.7).
func (ing *Ingester) SupportedExtensions() []string {
	if l, ok := ing.parser.(supportedExtensionLister); ok {
		return l.SupportedExtensions()
	}
	return nil
}

// supportedExtensionLister is the parser-side capability the cold scan reads
// to source its walk filter (solov2-xde2.7). Kept here next to its consumer
// (ISP) rather than widening the broad ports.CodeParser contract, which would
// ripple to every ParseFile implementer and test fake.
type supportedExtensionLister interface {
	SupportedExtensions() []string
}

// TracerProvider returns the installed TracerProvider, or nil if none was
// supplied. Exposed so the daemon wiring test can assert the tracer was
// threaded into every tracing-aware consumer.
func (ing *Ingester) TracerProvider() observability.TracerProvider {
	return ing.tp
}

// tracerProvider returns the configured provider or a noop if nil.
func (ing *Ingester) tracerProvider() observability.TracerProvider {
	if ing.tp == nil {
		return noop.NewTracerProvider()
	}
	return ing.tp
}

// Save parses src for the file at path and stages the result.
// repoID and branch scope the staging slot.
// Parse errors are non-fatal: a returned error from ParseFile is logged at
// WARN level and the file is simply not staged. Save itself does NOT touch
// SQLite for staging, but it does emit parse-failure / parse-clear /
// per-file todo findings via the configured FindingStorage.
func (ing *Ingester) Save(ctx context.Context, repoID, branch, path string, src []byte) {
	ing.save(ctx, repoID, branch, path, src, false /*coldScan*/)
}

// SaveColdScan is the cold-scan variant of Save. It still parses and
// stages and still emits parse-failure findings for syntactically broken
// files, but it skips clearParseFailure — the per-file no-op UPDATE that
// the regular Save path issues to revalidate an already-clean file
// (solov2-pc3 fix #2). On a 646-file cold scan against a fresh repo,
// this eliminates 646 needless writes through the contended Write
// pool. Per-file TODOs are still emitted because emitTodos has its own
// len==0 guard and is genuinely free for files without TODO markers.
func (ing *Ingester) SaveColdScan(ctx context.Context, repoID, branch, path string, src []byte) {
	ing.save(ctx, repoID, branch, path, src, true /*coldScan*/)
}

// save is the shared body. coldScan, when true, skips the
// clearParseFailure call on a successful clean parse — there's nothing
// to clear during a first-ever scan of a repo, and the per-file UPDATE
// dominates per-Save wall time under Write contention.
func (ing *Ingester) save(ctx context.Context, repoID, branch, path string, src []byte, coldScan bool) {
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
		return
	}
	ing.staging.Stage(repoID, branch, path, staging.File{
		Nodes:           result.Nodes,
		Edges:           result.Edges,
		UnresolvedCalls: result.UnresolvedCalls,
		Imports:         result.Imports,
	}, staging.WithGenerationGuard(gen, ing.gate))
	if len(result.Failures) == 0 {
		// fsnotify-driven Save path: a clean parse closes any
		// parse-failure finding the file carried from an earlier
		// broken ingest. Cold-scan path skips: there's no prior
		// finding to clear on a first-ever scan, and the per-file
		// UPDATE is the dominant cost (solov2-pc3 #2).
		if !coldScan {
			ing.emitter.ClearParseFailure(ctx, repoID, branch, path)
		}
	} else {
		ing.emitter.ParseFailures(ctx, repoID, branch, path, result.Failures)
	}
	ing.emitter.Todos(ctx, repoID, branch, path, result.Todos)
}

// DeleteFile removes the file from staging. Called when a file is deleted on disk.
func (ing *Ingester) DeleteFile(repoID, branch, path string) {
	ing.staging.DeleteStagedFile(repoID, branch, path)
}
