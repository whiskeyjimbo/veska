// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// Ingester parses files during file write events and stages the parsed
// representation. Writing staging data goes to the in-memory staging area,
// while parse failures are sent as findings to database storage.
type Ingester struct {
	parser  ports.CodeParser
	staging *staging.Area
	gate    *staging.Gate
	tp      observability.TracerProvider
	emitter *findingEmitter
}

type IngesterOption func(*Ingester)

func WithIngesterTracerProvider(tp observability.TracerProvider) IngesterOption {
	return func(ing *Ingester) { ing.tp = tp }
}

// WithFindingStorage configures the sink to persist parse failures and task/TODO
// occurrences.
func WithFindingStorage(s ports.FindingStorage) IngesterOption {
	return func(ing *Ingester) { ing.emitter = newFindingEmitter(s) }
}

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

// SupportedExtensions returns files extensions readable by the parser to let the
// cold scan filter files during walk.
func (ing *Ingester) SupportedExtensions() []string {
	if l, ok := ing.parser.(supportedExtensionLister); ok {
		return l.SupportedExtensions()
	}
	return nil
}

// supportedExtensionLister defines the optional extension query capabilities for
// code parsers.
type supportedExtensionLister interface {
	SupportedExtensions() []string
}

func (ing *Ingester) TracerProvider() observability.TracerProvider {
	return ing.tp
}

func (ing *Ingester) tracerProvider() observability.TracerProvider {
	if ing.tp == nil {
		return noop.NewTracerProvider()
	}
	return ing.tp
}

// Save parses src for the file at path and stages the result.
// repoID and branch scope the staging slot.
// Parse errors log as warnings without staging the file, reporting failure
// findings to database storage.
func (ing *Ingester) Save(ctx context.Context, repoID, branch, path string, src []byte) {
	ing.save(ctx, repoID, branch, path, src, false /*coldScan*/)
}

// SaveColdScan stages cold-scan parses but skips clearing parse failures. This
// avoids redundant database updates during fresh repository indexing.
func (ing *Ingester) SaveColdScan(ctx context.Context, repoID, branch, path string, src []byte) {
	ing.save(ctx, repoID, branch, path, src, true /*coldScan*/)
}

func (ing *Ingester) save(ctx context.Context, repoID, branch, path string, src []byte, coldScan bool) {
	// Block if a branch switch is in progress, querying generation limits before parsing.
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
		TypeRels:        result.TypeRels,
	}, staging.WithGenerationGuard(gen, ing.gate))
	if len(result.Failures) == 0 {
		// fsnotify file saves clear existing parse-failure findings, whereas cold scan
		// walks skip clearing to save database writes.
		if !coldScan {
			ing.emitter.ClearParseFailure(ctx, repoID, branch, path)
		}
	} else {
		ing.emitter.ParseFailures(ctx, repoID, branch, path, result.Failures)
	}
	ing.emitter.Todos(ctx, repoID, branch, path, result.Todos)
}

// DeleteFile stages a tombstone (an empty parse result) for a removed file so
// the next promotion runs its normal per-file replace path: it deletes the
// file's existing nodes/edges, prunes their vectors, and clears their FTS rows.
// Staging an empty entry - rather than dropping the path from staging - is what
// makes the deletion reach the database; a bare staging removal would leave the
// gone file's nodes (and embeddings) orphaned in the graph until a full re-scan.
// Any parse-failure finding for the path is cleared too, since the file no
// longer exists to re-parse.
func (ing *Ingester) DeleteFile(ctx context.Context, repoID, branch, path string) {
	ing.gate.WaitIfPaused()
	gen := ing.gate.Generation()
	ing.staging.Stage(repoID, branch, path, staging.File{}, staging.WithGenerationGuard(gen, ing.gate))
	ing.emitter.ClearParseFailure(ctx, repoID, branch, path)
}
