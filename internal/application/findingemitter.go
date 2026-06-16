package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// findingEmitter turns parse output into structural Findings on the configured
// FindingStorage sink. It is the half of ingest that "surfaces structural
// problems"; the Ingester owns parse→stage and delegates emission here so the
// two concerns can evolve independently (severity policy, message format, and
// routing all live in this one type).
// Emission is best-effort across every method: a nil sink is a no-op (the
// caller does not require one) and per-call errors are logged but never
// propagated — the Save hot path must not fail because a finding could not be
// persisted.
type findingEmitter struct {
	findings ports.FindingStorage
}

// newFindingEmitter builds an emitter over the given sink. A nil sink yields an
// emitter whose methods are all no-ops, so callers need not guard each call.
func newFindingEmitter(findings ports.FindingStorage) *findingEmitter {
	return &findingEmitter{findings: findings}
}

// ParseFailures forwards each ParseFailure to the sink as a single
// 'parse-failure' Finding anchored to the file.
// All failures for one (repo, branch, file) collapse to one finding_id because
// domain.NewFinding hashes (rule, anchor) only; the SQLite repo's
// ON CONFLICT(finding_id, branch) clause then guarantees at most one OPEN row.
func (e *findingEmitter) ParseFailures(ctx context.Context, repoID, branch, path string, failures []domain.ParseFailure) {
	if len(failures) == 0 || e.findings == nil {
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

	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID:   repoID,
		Branch:   branch,
		Severity: domain.SeverityMedium,
		Layer:    domain.LayerStructural,
		Rule:     "parse-failure",
		Message:  msg,
	}, domain.WithFileAnchor(path))
	if err != nil {
		slog.Warn("ingester: failed to construct parse-failure finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
		return
	}
	if err := e.findings.Save(ctx, f); err != nil {
		slog.Warn("ingester: failed to save parse-failure finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
	}
}

// ClearParseFailure closes any OPEN parse-failure finding for (repoID, branch,
// path) once the file parses cleanly. It reconstructs the branch-stable
// FindingID by building the same finding ParseFailures would build
// domain.NewFinding hashes only (rule, anchor), so the message body is
// irrelevant — then asks the sink to close it.
func (e *findingEmitter) ClearParseFailure(ctx context.Context, repoID, branch, path string) {
	if e.findings == nil {
		return
	}

	// Message body is irrelevant to FindingID; use a placeholder so
	// NewFinding's non-empty validation is satisfied.
	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID:   repoID,
		Branch:   branch,
		Severity: domain.SeverityMedium,
		Layer:    domain.LayerStructural,
		Rule:     "parse-failure",
		Message:  "parse failure cleared",
	}, domain.WithFileAnchor(path))
	if err != nil {
		slog.Warn("ingester: failed to construct parse-failure finding for clear",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
		return
	}
	if err := e.findings.CloseObsolete(ctx, f.FindingID, branch); err != nil {
		slog.Warn("ingester: failed to close parse-failure finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
	}
}

// Todos forwards parser-detected TODO/FIXME markers as a single 'todo' finding
// anchored on the file. Multiple markers within the same file collapse to one
// finding row (the finding_id is hashed on (rule, anchor) so re-saving is
// idempotent); the message body lists every flagged line so callers can route
// the user straight to it.
func (e *findingEmitter) Todos(ctx context.Context, repoID, branch, path string, todos []domain.ParseTodo) {
	if len(todos) == 0 || e.findings == nil {
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

	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID:   repoID,
		Branch:   branch,
		Severity: domain.SeverityInfo,
		Layer:    domain.LayerStructural,
		Rule:     "todo",
		Message:  msg,
	}, domain.WithFileAnchor(path))
	if err != nil {
		slog.Warn("ingester: failed to construct todo finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
		return
	}
	if err := e.findings.Save(ctx, f); err != nil {
		slog.Warn("ingester: failed to save todo finding",
			"repoID", repoID, "branch", branch, "path", path, "err", err)
	}
}
