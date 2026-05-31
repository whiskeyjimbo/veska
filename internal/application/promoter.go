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

// CheckRunInput is the data the post-commit check pipeline receives. It mirrors
// internal/application/checks.Input but is declared here so that Promoter can
// reference the runner without importing the sub-package (preventing an import
// cycle: checks already depends on observability + ports, and application
// imports checks would force application's tests to drag in metrics setup for
// every promoter test).
type CheckRunInput struct {
	RepoID    string
	Branch    string
	GitSHA    string
	FilePaths []string
	// AddedLines holds the newly-added ("+") lines introduced by the
	// promoted commit, keyed by repo-root-relative file path. It is
	// populated once by the promotion path (via an AddedLinesFunc seam)
	// so each check stays a pure function of its input; checks that do
	// not need diff data simply ignore it. May be nil when no seam is
	// installed.
	AddedLines map[string][]Line
}

// Line is a single newly-added line of a commit's diff: its line number
// in the post-commit revision plus the line text (no leading "+" marker,
// no trailing newline). It mirrors checks.Line and git.Line; the type is
// re-declared here so the application package need not import either —
// consistent with how CheckRunInput mirrors checks.Input.
type Line struct {
	Number int
	Text   string
}

// AddedLinesFunc resolves the newly-added lines of a promoted commit.
// It is the application-layer seam over git diff parsing: the concrete
// implementation (git.AddedLinesForCommit) lives in infrastructure and is
// injected in wire.go, keeping Promoter free of infrastructure imports.
type AddedLinesFunc func(ctx context.Context, repoID, gitSHA string) (map[string][]Line, error)

// CheckRunner is the contract Promoter requires from the post-commit
// structural check pipeline. The concrete implementation lives in
// internal/application/checks. CheckRunner.Run is invoked AFTER the promotion
// transaction commits and MUST NOT return an error — findings are advisory and
// cannot abort a promotion.
type CheckRunner interface {
	Run(ctx context.Context, in CheckRunInput)
}

// Promoter flushes the in-memory staging.Area to durable storage via a
// PromotionStore and enqueues post-promotion work items. It is a thin
// orchestrator: it builds a PromotionBatch from the staging snapshot, delegates
// the atomic transaction to the store, then performs advisory post-commit work
// (staging cleanup, structural checks).
//
// Promoter no longer writes SQL itself — all durable writes flow through the
// PromotionStore port, keeping the application layer free of database/sql.
type Promoter struct {
	staging *staging.Area
	store   PromotionStore
	tp      observability.TracerProvider
	checks  CheckRunner
	added   AddedLinesFunc
}

// PromoterOption configures optional Promoter collaborators at construction.
// The required staging/store are positional; the post-commit seams (check
// runner, added-lines resolver, tracer) are options so the constructed Promoter
// is immutable and fully wired before use.
type PromoterOption func(*Promoter)

// WithCheckRunner installs the post-commit structural check runner. The runner
// is invoked after the promotion transaction commits, before Promote returns.
// If omitted, no checks run.
func WithCheckRunner(r CheckRunner) PromoterOption {
	return func(p *Promoter) { p.checks = r }
}

// WithAddedLinesFunc installs the seam that resolves the newly-added lines of a
// promoted commit. When set, Promote calls it after commit and passes the
// result on CheckRunInput.AddedLines. If omitted, AddedLines is left nil and
// checks that need diff data are skipped.
func WithAddedLinesFunc(f AddedLinesFunc) PromoterOption {
	return func(p *Promoter) { p.added = f }
}

// WithPromoterTracerProvider installs a TracerProvider for promotion.transaction
// spans. If omitted (or given nil), a noop provider is used.
func WithPromoterTracerProvider(tp observability.TracerProvider) PromoterOption {
	return func(p *Promoter) { p.tp = tp }
}

// NewPromoter constructs a Promoter wired to the provided staging.Area and
// PromotionStore. The store owns the promotion transaction; the Promoter only
// orchestrates the snapshot and advisory post-commit steps. Optional seams
// (check runner, added-lines resolver, tracer) are supplied via PromoterOption.
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

// TracerProvider returns the installed TracerProvider, or nil if none was
// supplied. Exposed so the daemon wiring test can assert the tracer was
// threaded into every tracing-aware consumer.
func (p *Promoter) TracerProvider() observability.TracerProvider {
	return p.tp
}

// tracerProvider returns the configured provider or a noop if nil.
func (p *Promoter) tracerProvider() observability.TracerProvider {
	if p.tp == nil {
		return noop.NewTracerProvider()
	}
	return p.tp
}

// Promote is called by the post-commit hook.  It:
//  1. Takes a snapshot of all nodes staged for (repoID, branch).
//  2. Builds a PromotionBatch and hands it to the PromotionStore, which writes
//     all node/FTS/embedding-ref/queue rows in a single atomic transaction.
//  3. Calls staging.Area.DeleteStagedFile for each promoted file after commit.
//  4. Writes advisory audit entries and runs post-commit structural checks.
//
// Node-only promotion: edges are intentionally not promoted here. They are
// re-derived post-promotion by the auto_link queue worker (work_kind="auto_link").
// Staged edges remain in the staging.Area solely to serve pre-promotion overlay reads.
//
// actor records who triggered the promotion. Hook-triggered paths should pass
// domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}.
func (p *Promoter) Promote(ctx context.Context, repoID, branch, gitSHA string, actor domain.Actor) error {
	// Operators tail daemon.log to confirm "did my last commit get picked
	// up?". Mirror coldscan's 'starting' / 'complete' INFO pair so that
	// signal exists for promotions too .
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

	// The promotion.transaction span wraps the atomic store write. The store
	// owns the transaction; this thin span preserves end-to-end tracing of the
	// commit phase. An empty batch still reaches the store so the registration
	// check runs — an unregistered repo is rejected even with nothing staged —
	// but the store opens no transaction for an empty batch.
	ctx, span := observability.StartSpan(ctx, p.tracerProvider(), "promotion.transaction")
	err := p.store.Promote(ctx, batch)
	span.End()
	if err != nil {
		slog.Error("promotion: failed",
			"repo_id", repoID, "branch", branch, "git_sha", gitSHA,
			"err", err,
			"elapsed_ms", time.Since(promoteStart).Milliseconds(),
		)
		return err
	}

	// Nothing was staged: registration was confirmed, but there is no
	// post-commit work to do — skip staging cleanup, audit, and checks.
	if len(batch.Files) == 0 {
		slog.Info("promotion: complete",
			"repo_id", repoID, "branch", branch, "git_sha", gitSHA,
			"files", 0,
			"elapsed_ms", time.Since(promoteStart).Milliseconds(),
		)
		return nil
	}

	// Clear staging entries only after a successful commit.
	filePaths := make([]string, 0, len(batch.Files))
	for _, f := range batch.Files {
		filePaths = append(filePaths, f.Path)
		p.staging.DeleteStagedFile(repoID, branch, f.Path)
	}

	// Post-commit: run advisory structural checks against the just-committed
	// slice of the graph. Findings cannot abort the promotion — by contract
	// the runner does not return an error.
	if p.checks != nil {
		// Resolve the commit's added lines via the injected seam. A seam
		// error is non-fatal — like findings, diff data is advisory: the
		// checks still run, just without per-line diff context.
		var addedLines map[string][]Line
		if p.added != nil {
			if al, err := p.added(ctx, repoID, gitSHA); err == nil {
				addedLines = al
			}
		}
		p.checks.Run(ctx, CheckRunInput{
			RepoID:     repoID,
			Branch:     branch,
			GitSHA:     gitSHA,
			FilePaths:  filePaths,
			AddedLines: addedLines,
		})
	}

	slog.Info("promotion: complete",
		"repo_id", repoID, "branch", branch, "git_sha", gitSHA,
		"files", len(batch.Files),
		"elapsed_ms", time.Since(promoteStart).Milliseconds(),
	)
	return nil
}
