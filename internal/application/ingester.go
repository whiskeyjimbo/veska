package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// Ingester orchestrates the parse-on-save hot path:
// CodeParser.ParseFile → StagingArea write.
// It never touches SQLite for graph writes; all graph state goes to the
// in-memory StagingArea only. Parse failures, however, are surfaced as
// Findings through the optional FindingStorage port (set via
// SetFindingStorage) so structural problems are visible to operators before
// commit-seal time.
type Ingester struct {
	parser  ports.CodeParser
	staging *StagingArea
	gate    *IngestionGate
	tp      observability.TracerProvider

	mu       sync.RWMutex
	findings ports.FindingStorage
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
	ing.mu.Lock()
	ing.findings = s
	ing.mu.Unlock()
}

// findingStorage returns the currently installed FindingStorage, or nil if none.
func (ing *Ingester) findingStorage() ports.FindingStorage {
	ing.mu.RLock()
	defer ing.mu.RUnlock()
	return ing.findings
}

// TracerProvider returns the installed TracerProvider, or nil if none has
// been set. It is the read companion to SetTracerProvider.
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
	ing.staging.Stage(repoID, branch, path, StagedFile{
		Nodes:           result.Nodes,
		Edges:           result.Edges,
		UnresolvedCalls: result.UnresolvedCalls,
		Imports:         result.Imports,
	}, WithGenerationGuard(gen, ing.gate))
	if len(result.Failures) == 0 {
		// fsnotify-driven Save path: a clean parse closes any
		// parse-failure finding the file carried from an earlier
		// broken ingest. Cold-scan path skips: there's no prior
		// finding to clear on a first-ever scan, and the per-file
		// UPDATE is the dominant cost (solov2-pc3 #2).
		if !coldScan {
			ing.clearParseFailure(ctx, repoID, branch, path)
		}
	} else {
		ing.emitParseFailures(ctx, repoID, branch, path, result.Failures)
	}
	ing.emitTodos(ctx, repoID, branch, path, result.Todos)
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

// clearParseFailure closes any OPEN parse-failure finding for (repoID, branch,
// path) once the file parses cleanly. It reconstructs the branch-stable
// FindingID by building the same finding emitParseFailures would build —
// domain.NewFinding hashes only (rule, anchor), so the message body is
// irrelevant — then asks the sink to close it.
//
// Emission is best-effort, matching emitParseFailures: a nil sink is a no-op
// and per-call errors are logged but not propagated — the Save hot path must
// not fail because a finding could not be closed.
func (ing *Ingester) clearParseFailure(ctx context.Context, repoID, branch, path string) {
	sink := ing.findingStorage()
	if sink == nil {
		return
	}

	// Message body is irrelevant to FindingID; use a placeholder so
	// NewFinding's non-empty validation is satisfied.
	f, err := domain.NewFinding(
		repoID, branch,
		domain.SeverityMedium,
		domain.LayerStructural,
		"parse-failure",
		"parse failure cleared",
		domain.WithFileAnchor(path),
	)
	if err != nil {
		slog.Warn("ingester: failed to construct parse-failure finding for clear",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
		return
	}
	if err := sink.CloseObsolete(ctx, f.FindingID, branch); err != nil {
		slog.Warn("ingester: failed to close parse-failure finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
	}
}

// emitTodos forwards parser-detected TODO/FIXME markers as a single
// 'todo' finding anchored on the file. Multiple markers within the same
// file collapse to one finding row (the finding_id is hashed on (rule,
// anchor) so re-saving is idempotent); the message body lists every
// flagged line so callers can route the user straight to it.
//
// Emission is best-effort, matching emitParseFailures: a nil sink is a
// no-op and per-call errors are logged but not propagated.
func (ing *Ingester) emitTodos(ctx context.Context, repoID, branch, path string, todos []domain.ParseTodo) {
	if len(todos) == 0 {
		return
	}
	sink := ing.findingStorage()
	if sink == nil {
		return
	}

	// Build a compact summary message. We list up to the first few markers
	// inline; if there are more we suffix with a count to keep the message
	// bounded.
	const inlineCap = 5
	parts := make([]string, 0, inlineCap)
	for i, t := range todos {
		if i >= inlineCap {
			break
		}
		parts = append(parts, fmt.Sprintf("L%d: %s", t.Line, t.Message))
	}
	summary := strings.Join(parts, "; ")
	if len(todos) > inlineCap {
		summary = fmt.Sprintf("%s; (+%d more)", summary, len(todos)-inlineCap)
	}
	msg := fmt.Sprintf("%d TODO/FIXME marker(s) in %s: %s", len(todos), path, summary)

	f, err := domain.NewFinding(
		repoID, branch,
		domain.SeverityInfo,
		domain.LayerStructural,
		"todo",
		msg,
		domain.WithFileAnchor(path),
	)
	if err != nil {
		slog.Warn("ingester: failed to construct todo finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
		return
	}
	if err := sink.Save(ctx, f); err != nil {
		slog.Warn("ingester: failed to save todo finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
	}
}

// DeleteFile removes the file from staging. Called when a file is deleted on disk.
func (ing *Ingester) DeleteFile(repoID, branch, path string) {
	ing.staging.DeleteStagedFile(repoID, branch, path)
}
