package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
}

// NewHandler constructs a review Handler. gen, loader and repoRoot are all
// required; a nil dependency yields an error wrapping ErrMissingDependency and
// a nil *Handler.
func NewHandler(gen ports.LLMGenerator, loader *Loader, repoRoot RepoRootFunc) (*Handler, error) {
	if gen == nil {
		return nil, fmt.Errorf("review.NewHandler: gen is nil: %w", ErrMissingDependency)
	}
	if loader == nil {
		return nil, fmt.Errorf("review.NewHandler: loader is nil: %w", ErrMissingDependency)
	}
	if repoRoot == nil {
		return nil, fmt.Errorf("review.NewHandler: repoRoot is nil: %w", ErrMissingDependency)
	}
	return &Handler{gen: gen, loader: loader, repoRoot: repoRoot}, nil
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
		return fmt.Errorf("review.Handle: unexpected kind %q", row.Kind)
	}
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
