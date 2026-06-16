package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// findingEmitter translates parsing errors and annotations into database-persisted
// Findings. Emission is best-effort: a nil sink no-ops, and failures are logged
// rather than propagated to keep the Save hot path infallible.
type findingEmitter struct {
	findings ports.FindingStorage
}


func newFindingEmitter(findings ports.FindingStorage) *findingEmitter {
	return &findingEmitter{findings: findings}
}

// ParseFailures emits syntax error findings. All failures for a given file
// collapse to a single finding ID because domain.NewFinding hashes the rule
// and file path, guaranteeing at most one open failure row.
func (e *findingEmitter) ParseFailures(ctx context.Context, repoID, branch, path string, failures []domain.ParseFailure) {
	if len(failures) == 0 || e.findings == nil {
		return
	}

	// Only the first syntax error is surfaced to keep the finding stable against
	// parser cascades.
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

// ClearParseFailure closes open parse-failure findings once a file parses
// cleanly, using a placeholder message because ID resolution depends only on
// rule and file path hashes.
func (e *findingEmitter) ClearParseFailure(ctx context.Context, repoID, branch, path string) {
	if e.findings == nil {
		return
	}


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

// Todos forwards TODO/FIXME markers as a single finding anchored to the file,
// using ID-hashing for idempotency and listing all locations in the message.
func (e *findingEmitter) Todos(ctx context.Context, repoID, branch, path string, todos []domain.ParseTodo) {
	if len(todos) == 0 || e.findings == nil {
		return
	}


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
