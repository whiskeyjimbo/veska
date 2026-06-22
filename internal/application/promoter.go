// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"context"
	"log/slog"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
	"go.opentelemetry.io/otel/trace/noop"
)

// CheckRunInput holds parameters for the post-commit structural check pipeline,
// avoiding a dependency cycle with the checks sub-package.
type CheckRunInput struct {
	RepoID    string
	Branch    string
	GitSHA    string
	FilePaths []string
	// AddedLines maps newly-added lines from git diffs per file path so checks can
	// run as pure functions of their inputs.
	AddedLines map[string][]Line
}

// Line mirrors diff records from git to isolate the application layer from git
// adapter models.
type Line struct {
	Number int
	Text   string
}

// AddedLinesFunc defines the callback signature to resolve added lines without
// dragging in git infrastructure imports.
type AddedLinesFunc func(ctx context.Context, repoID, gitSHA string) (map[string][]Line, error)

// CheckRunner runs structural checks after the promotion transaction commits;
// failures are advisory and do not abort promotions.
type CheckRunner interface {
	Run(ctx context.Context, in CheckRunInput)
}

// Promoter orchestrates the staging-to-storage flush and queues post-promotion
// work. All persistent writes flow through the PromotionStore port to keep the
// package database-agnostic.
type Promoter struct {
	staging *staging.Area
	store   PromotionStore
	tp      observability.TracerProvider
	checks  CheckRunner
	added   AddedLinesFunc
}

type PromoterOption func(*Promoter)

func WithCheckRunner(r CheckRunner) PromoterOption {
	return func(p *Promoter) { p.checks = r }
}

func WithAddedLinesFunc(f AddedLinesFunc) PromoterOption {
	return func(p *Promoter) { p.added = f }
}

func WithPromoterTracerProvider(tp observability.TracerProvider) PromoterOption {
	return func(p *Promoter) { p.tp = tp }
}

func NewPromoter(area *staging.Area, store PromotionStore, opts ...PromoterOption) *Promoter {
	p := &Promoter{
		staging: area,
		store:   store,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *Promoter) TracerProvider() observability.TracerProvider {
	return p.tp
}

func (p *Promoter) tracerProvider() observability.TracerProvider {
	if p.tp == nil {
		return noop.NewTracerProvider()
	}
	return p.tp
}

// Promote commits staged files to the store and runs advisory structural
// checks. Staged edges remain in staging solely to serve pre-promotion overlay
// reads.
func (p *Promoter) Promote(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error {
	// Log promotion start and completion to allow operators to trace processing pipeline stages.
	promoteStart := time.Now()
	snap := p.staging.Snapshot(repoID, branch)
	slog.Info("promotion: starting",
		"repo_id", repoID,
		"branch", branch,
		"git_sha", gitSHA,
		"files", len(snap),
	)

	now := time.Now().UnixMilli()
	batch := PromotionBatch{
		RepoID:     repoID,
		Branch:     branch,
		GitSHA:     gitSHA,
		Actor:      actor,
		PromotedAt: now,
		Files:      make([]PromotionFile, 0, len(snap)),
	}
	for filePath, sf := range snap {
		batch.Files = append(batch.Files, PromotionFile{
			Path:            filePath,
			Nodes:           sf.Nodes,
			Edges:           sf.Edges,
			UnresolvedCalls: sf.UnresolvedCalls,
			Imports:         sf.Imports,
		})
	}

	// Start a tracing span to benchmark the atomic promotion transaction. Empty
	// batches are still sent to the store to run repo registration guards.
	ctx, span := observability.StartSpan(ctx, p.tracerProvider(), "promotion.transaction")
	txnStart := time.Now()
	err := p.store.Promote(ctx, batch)
	txnMS := time.Since(txnStart).Milliseconds()
	span.End()
	if err != nil {
		slog.Error("promotion: failed",
			"repo_id", repoID, "branch", branch, "git_sha", gitSHA,
			"err", err,
			"elapsed_ms", time.Since(promoteStart).Milliseconds(),
		)
		return err
	}

	if len(batch.Files) == 0 {
		slog.Info("promotion: complete",
			"repo_id", repoID, "branch", branch, "git_sha", gitSHA,
			"files", 0,
			"elapsed_ms", time.Since(promoteStart).Milliseconds(),
		)
		return nil
	}

	// staging.Area cleanup occurs only after a successful transaction commit to prevent data
	// loss on rollback.
	filePaths := make([]string, 0, len(batch.Files))
	for _, f := range batch.Files {
		filePaths = append(filePaths, f.Path)
		p.staging.DeleteStagedFile(repoID, branch, f.Path)
	}

	// Split the post-transaction cost so perf work can see where the promote
	// phase spends its time: added-lines fetch vs the structural-check pipeline.
	var addedMS, checksMS int64
	if p.checks != nil {
		// Seam errors when fetching added lines are ignored since checks can degrade
		// gracefully to run without diff context.
		var addedLines map[string][]Line
		if p.added != nil {
			addedStart := time.Now()
			if al, err := p.added(ctx, repoID, gitSHA); err == nil {
				addedLines = al
			}
			addedMS = time.Since(addedStart).Milliseconds()
		}
		checksStart := time.Now()
		p.checks.Run(ctx, CheckRunInput{
			RepoID:     repoID,
			Branch:     branch,
			GitSHA:     gitSHA,
			FilePaths:  filePaths,
			AddedLines: addedLines,
		})
		checksMS = time.Since(checksStart).Milliseconds()
	}

	slog.Info("promotion: complete",
		"repo_id", repoID, "branch", branch, "git_sha", gitSHA,
		"files", len(batch.Files),
		"txn_ms", txnMS,
		"added_ms", addedMS,
		"checks_ms", checksMS,
		"elapsed_ms", time.Since(promoteStart).Milliseconds(),
	)
	return nil
}
