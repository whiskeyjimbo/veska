package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by NewHandler when a required collaborator
// is nil. It is matched with errors.Is by callers.
var ErrMissingDependency = errors.New("review: missing required dependency")

// RepoRootFunc resolves a repoID to its registered working-tree path. It
// mirrors the wiki handler's resolver so the review handler can turn a
// repoRoot-relative changed-file path (the queue row payload) into an absolute
// path readable from disk.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// Handler implements ports.WorkHandler for WorkKindReview rows. One row maps to
// one changed file: the handler reads the file's current source, renders each
// review prompt over it, dispatches the rendered text through the LLMGenerator,
// and parses the response.
//
// It is a thin orchestrator. Producing domain.Findings from the parsed
// ReviewFindings (solov2-nz2.6), the elaborate review-pipeline-failure contract
// (solov2-nz2.3), and the token quota (solov2-nz2.5) are NOT handled here: a
// returned error is enough for queue.Poller's existing retry path, and a
// successful Parse drains the row to 'done'.
//
// The handler is stateless beyond its injected dependencies; the Loader and
// LLMGenerator are read-only/concurrency-safe, so the handler is safe for the
// poller's per-kind goroutine.
type Handler struct {
	gen      ports.LLMGenerator
	loader   *Loader
	repoRoot RepoRootFunc
	findings ports.FindingStorage
}

// NewHandler constructs a review Handler. gen, loader, repoRoot and findings
// are all required; a nil dependency yields an error wrapping
// ErrMissingDependency and a nil *Handler.
//
// findings is the port the Handler uses to park a review-pipeline-failure
// Finding when a job exhausts its retries (the review failure contract,
// solov2-nz2.3).
func NewHandler(gen ports.LLMGenerator, loader *Loader, repoRoot RepoRootFunc, findings ports.FindingStorage) (*Handler, error) {
	if gen == nil {
		return nil, fmt.Errorf("review.NewHandler: gen is nil: %w", ErrMissingDependency)
	}
	if loader == nil {
		return nil, fmt.Errorf("review.NewHandler: loader is nil: %w", ErrMissingDependency)
	}
	if repoRoot == nil {
		return nil, fmt.Errorf("review.NewHandler: repoRoot is nil: %w", ErrMissingDependency)
	}
	if findings == nil {
		return nil, fmt.Errorf("review.NewHandler: findings is nil: %w", ErrMissingDependency)
	}
	return &Handler{gen: gen, loader: loader, repoRoot: repoRoot, findings: findings}, nil
}

// Handle processes one ports.WorkRow of kind WorkKindReview.
//
// Behaviour:
//   - Wrong kind: wrapped error (routing bug).
//   - Empty payload: nil (no file => nothing to review).
//   - Repo-root resolution / file-read error: wrapped error so the Poller
//     retries.
//   - Per review kind: load the prompt, Render the file's source, Generate,
//     Parse. ErrEmptyInput from Render skips that kind cleanly.
//   - On success: nil — the row drains to 'done'.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindReview {
		// A misrouted row is a wiring bug, not a review failure — no finding.
		return fmt.Errorf("review.Handle: unexpected kind %q", row.Kind)
	}

	err := h.review(ctx, row)
	if err == nil {
		return nil
	}

	// Review failure contract (solov2-nz2.3): on the FINAL failing attempt the
	// job will be marked state='failed' by the poller. Park a sticky
	// review-pipeline-failure Finding so the failure does not silently vanish.
	// Earlier attempts just return the error for the poller to re-queue.
	if row.Attempts >= maxReviewAttempts {
		h.emitFailureFinding(ctx, row, err)
	}
	return err
}

// review runs the review job for one queue row. The returned error is the
// job failure the poller acts on; emitting the failure Finding is the caller's
// concern.
func (h *Handler) review(ctx context.Context, row ports.WorkRow) error {
	filePath := row.Payload
	if filePath == "" {
		return nil
	}

	root, err := h.repoRoot(ctx, row.RepoID)
	if err != nil {
		return fmt.Errorf("review.Handle: resolve repo root for %q: %w", row.RepoID, err)
	}

	src, err := os.ReadFile(filepath.Join(root, filePath))
	if err != nil {
		return fmt.Errorf("review.Handle: read changed file %q: %w", filePath, err)
	}

	in := Input{
		RepoID:   row.RepoID,
		Branch:   row.Branch,
		FilePath: filePath,
		Code:     string(src),
	}

	for _, kind := range h.loader.Kinds() {
		if err := h.runKind(ctx, kind, in); err != nil {
			return err
		}
	}
	return nil
}

// emitFailureFinding parks a review-pipeline-failure Finding anchored on the
// promotion commit (row.GitSHA). FindingStorage.Save is idempotent on
// (finding_id, branch), so multiple failed review files in one commit collapse
// to a single finding. A Save error must never mask the original job failure:
// it is logged and the job error still propagates.
func (h *Handler) emitFailureFinding(ctx context.Context, row ports.WorkRow, jobErr error) {
	msg := fmt.Sprintf("review pipeline failed for %q after %d attempts: %v",
		row.Payload, row.Attempts, jobErr)

	f, err := domain.NewFinding(
		"", row.RepoID, row.Branch,
		domain.SeverityHigh, domain.LayerQuality,
		FailureRule, msg,
		domain.WithNodeAnchor(row.GitSHA),
		domain.WithActorKind(domain.ActorKindSystem),
	)
	if err != nil {
		slog.Error("review: build failure finding",
			"repo_id", row.RepoID, "branch", row.Branch, "git_sha", row.GitSHA, "err", err)
		return
	}
	if err := h.findings.Save(ctx, f); err != nil {
		slog.Error("review: save failure finding",
			"repo_id", row.RepoID, "branch", row.Branch, "git_sha", row.GitSHA, "err", err)
	}
}

// runKind dispatches a single review kind over the code-under-review: load the
// versioned prompt, Render, Generate (echoing the prompt version into the
// request), then Parse. An empty-code Input is skipped cleanly.
func (h *Handler) runKind(ctx context.Context, kind ReviewKind, in Input) error {
	prompt, err := h.loader.LoadPrompt(kind)
	if err != nil {
		return fmt.Errorf("review.Handle: load prompt %q: %w", kind, err)
	}

	rendered, err := prompt.Render(in)
	if errors.Is(err, ErrEmptyInput) {
		// Nothing to review for this kind — not a failure.
		return nil
	}
	if err != nil {
		return fmt.Errorf("review.Handle: render prompt %q: %w", kind, err)
	}

	resp, err := h.gen.Generate(ctx, ports.GenerateRequest{
		Prompt:                rendered,
		PromptTemplateVersion: prompt.Version(),
	})
	if err != nil {
		return fmt.Errorf("review.Handle: generate for %q: %w", kind, err)
	}

	// Parse validates the response shape. Producing domain.Findings from the
	// parsed ReviewFindings is solov2-nz2.6 — this handler discards them.
	if _, err := prompt.Parse(resp.Text); err != nil {
		return fmt.Errorf("review.Handle: parse response for %q: %w", kind, err)
	}
	return nil
}

// Compile-time check: *Handler satisfies ports.WorkHandler.
var _ ports.WorkHandler = (*Handler)(nil)
