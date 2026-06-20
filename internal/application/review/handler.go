// SPDX-License-Identifier: AGPL-3.0-only

package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by NewHandler when a required collaborator
// is nil. It is matched with errors.Is by callers.
var ErrMissingDependency = errors.New("review: missing required dependency")

// ErrQuotaExceeded is returned by Handle when a review job is refused for a
// token-budget overage (per-commit cap reached) or paused (per-day cap
// reached). It is matched with errors.Is; its message carries the
// review_quota_exceeded degraded reason so the poller's degraded path can
// surface it.
var ErrQuotaExceeded = errors.New(quotaExceeded)

// RepoRootFunc resolves a repoID to its registered working-tree path. It
// mirrors the wiki handler's resolver so the review handler can turn a
// repoRoot-relative changed-file path (the queue row payload) into an absolute
// path readable from disk.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// ErrorCounter is the minimal metrics surface the Handler needs to record a
// budget refusal. Defined here so the application layer does not import
// observability; the daemon wires in an adapter over Metrics.ErrorCount.
type ErrorCounter interface {
	// IncError increments the error counter for the given kind label.
	IncError(kind string)
}

// Handler implements ports.WorkHandler for WorkKindReview rows. One row maps to
// one changed file: the handler reads the file's current source, renders each
// review prompt over it, dispatches the rendered text through the LLMGenerator,
// and parses the response.
// When a Quota is wired in (WithQuota) the handler enforces the review token
// caps: it pauses before dispatch when the per-day total is reached, refuses
// remaining jobs for a commit once that commit's per-commit total is reached,
// and records each completed job's actual token usage.
// The handler is stateless beyond its injected dependencies; the Quota is
// concurrency-safe, so the handler is safe for the poller's per-kind goroutine.
type Handler struct {
	gen      ports.LLMGenerator
	loader   *Loader
	repoRoot RepoRootFunc
	findings ports.FindingStorage

	quota   *Quota
	audit   ports.AuditWriter
	metrics ErrorCounter

	// dailyPauseAudited records the local date the daily-cap pause audit line
	// was already written for, so the line is emitted at most once per day.
	dailyPauseAudited atomicString
}

// HandlerOption customizes a Handler at construction time.
type HandlerOption func(*Handler)

// WithQuota wires in the review token Quota. Without it the handler runs every
// job unconditionally (the pre-nz2.5 behavior).
func WithQuota(q *Quota) HandlerOption {
	return func(h *Handler) {
		if q != nil {
			h.quota = q
		}
	}
}

// WithAuditWriter wires in the AuditWriter used to record the one-line
// audit.jsonl entry when the daily-cap pause trips.
func WithAuditWriter(w ports.AuditWriter) HandlerOption {
	return func(h *Handler) {
		if w != nil {
			h.audit = w
		}
	}
}

// WithErrorCounter wires in the metrics counter incremented on a budget
// refusal (veska_error_count{kind="review"}).
func WithErrorCounter(c ErrorCounter) HandlerOption {
	return func(h *Handler) {
		if c != nil {
			h.metrics = c
		}
	}
}

// NewHandler constructs a review Handler. gen, loader, repoRoot and findings
// are all required; a nil dependency yields an error wrapping
// ErrMissingDependency and a nil *Handler. Token-quota enforcement is opt-in
// via WithQuota / WithAuditWriter / WithErrorCounter.
func NewHandler(gen ports.LLMGenerator, loader *Loader, repoRoot RepoRootFunc, findings ports.FindingStorage, opts ...HandlerOption) (*Handler, error) {
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
	h := &Handler{gen: gen, loader: loader, repoRoot: repoRoot, findings: findings}
	for _, opt := range opts {
		if opt != nil {
			opt(h)
		}
	}
	return h, nil
}

// Handle processes one ports.WorkRow of kind WorkKindReview.
// Behavior:
//
//	Wrong kind: wrapped error (routing bug).
//	Empty payload: nil (no file => nothing to review).
//	Daily-cap pause: when the per-day token total is reached the job is not
//	  dispatched; an ErrQuotaExceeded is returned (the row stays pending) and
//	  one audit.jsonl line is written the first time the pause trips.
//	Per-commit overage: once the commit's running total reaches the cap the
//	  job is refused (no LLM call), one budget-exceeded Finding is filed, and
//	  the review error metric is incremented.
//	Repo-root resolution / file-read error: wrapped error so the Poller
//	  retries.
//	On success: nil - the row drains to 'done'.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindReview {
		// A misrouted row is a wiring bug, not a review failure - no finding.
		return fmt.Errorf("review.Handle: unexpected kind %q", row.Kind)
	}

	if h.quota != nil {
		if refused, err := h.enforceQuota(ctx, row); refused || err != nil {
			return err
		}
	}

	err := h.review(ctx, row)
	if err == nil {
		return nil
	}

	// Review failure contract: on the FINAL failing attempt the
	// job will be marked state='failed' by the poller. Park a sticky
	// review-pipeline-failure Finding so the failure does not silently vanish.
	// Earlier attempts just return the error for the poller to re-queue.
	if row.Attempts >= maxReviewAttempts {
		h.emitFailureFinding(ctx, row, err)
	}
	return err
}

// enforceQuota applies the pre-dispatch token-budget checks. It returns
// refused=true together with a non-nil ErrQuotaExceeded when the job must not
// run; refused=false means the job may proceed. A store read failure is
// returned as the error so the poller retries rather than silently skipping
// the cap.
func (h *Handler) enforceQuota(ctx context.Context, row ports.WorkRow) (refused bool, err error) {
	// Per-day cap: pause before dispatch.
	paused, total, derr := h.quota.DailyPaused(ctx)
	if derr != nil {
		return false, fmt.Errorf("review.Handle: daily quota check: %w", derr)
	}
	if paused {
		h.auditDailyPause(ctx, row, total)
		return true, fmt.Errorf("review.Handle: daily token cap reached (%d tokens): %w", total, ErrQuotaExceeded)
	}

	// Per-commit cap: refuse the remaining jobs for an over-budget commit.
	if h.quota.CommitExceeded(row.GitSHA) {
		h.emitBudgetFinding(ctx, row)
		if h.metrics != nil {
			h.metrics.IncError("review")
		}
		return true, fmt.Errorf("review.Handle: per-commit token cap reached for %q: %w", row.GitSHA, ErrQuotaExceeded)
	}
	return false, nil
}

// auditDailyPause writes a single audit.jsonl line the first time the daily
// pause trips for the current local day. Subsequent paused jobs on the same
// day write nothing.
func (h *Handler) auditDailyPause(ctx context.Context, row ports.WorkRow, total int) {
	if h.audit == nil {
		return
	}
	date := h.quota.localDate()
	if !h.dailyPauseAudited.compareAndSwap(date) {
		return
	}
	entry := ports.AuditEntry{
		RepoID:    row.RepoID,
		ActorID:   "service:veska",
		ActorKind: domain.ActorKindSystem,
		Op:        "review.quota.daily_pause",
		TargetID:  date,
		Branch:    row.Branch,
		CreatedAt: h.quota.now(),
	}
	if err := h.audit.Write(ctx, entry); err != nil {
		slog.Error("review: write daily-pause audit line", "date", date, "err", err)
		// Re-arm so a later job retries the audit write.
		h.dailyPauseAudited.reset(date)
	}
}

// emitBudgetFinding parks a budget-exceeded Finding anchored on the promotion
// commit (row.GitSHA). FindingStorage.Save is idempotent on (finding_id,
// branch), so multiple refused files in one commit collapse to a single
// finding. A Save error never masks the refusal.
func (h *Handler) emitBudgetFinding(ctx context.Context, row ports.WorkRow) {
	msg := fmt.Sprintf("review refused for %q: per-commit token budget exceeded for commit %s",
		row.Payload, row.GitSHA)

	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID:   row.RepoID,
		Branch:   row.Branch,
		Severity: domain.SeverityMedium,
		Layer:    domain.LayerQuality,
		Rule:     BudgetRule,
		Message:  msg,
	},
		domain.WithNodeAnchor(row.GitSHA),
		domain.WithActorKind(domain.ActorKindSystem),
	)
	if err != nil {
		slog.Error("review: build budget finding",
			"repo_id", row.RepoID, "branch", row.Branch, "git_sha", row.GitSHA, "err", err)
		return
	}
	if err := h.findings.Save(ctx, f); err != nil {
		slog.Error("review: save budget finding",
			"repo_id", row.RepoID, "branch", row.Branch, "git_sha", row.GitSHA, "err", err)
	}
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
		if err := h.runKind(ctx, kind, in, row.GitSHA); err != nil {
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

	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID:   row.RepoID,
		Branch:   row.Branch,
		Severity: domain.SeverityHigh,
		Layer:    domain.LayerQuality,
		Rule:     FailureRule,
		Message:  msg,
	},
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
// request), then Parse. An empty-code Input is skipped cleanly. When a Quota
// is wired in, the actual token usage of each successful Generate call is
// recorded against the commit's per-commit and the day's per-day totals.
func (h *Handler) runKind(ctx context.Context, kind ReviewKind, in Input, gitSHA string) error {
	prompt, err := h.loader.LoadPrompt(kind)
	if err != nil {
		return fmt.Errorf("review.Handle: load prompt %q: %w", kind, err)
	}

	rendered, err := prompt.Render(in)
	if errors.Is(err, ErrEmptyInput) {
		// Nothing to review for this kind - not a failure.
		return nil
	}
	if err != nil {
		return fmt.Errorf("review.Handle: render prompt %q: %w", kind, err)
	}

	resp, err := h.gen.Generate(ctx, ports.GenerateRequest{
		Prompt:                rendered,
		PromptTemplateVersion: prompt.Version(),
		Format:                prompt.Format(),
	})
	if err != nil {
		return fmt.Errorf("review.Handle: generate for %q: %w", kind, err)
	}

	// Post-hoc accounting: add the job's ACTUAL token usage to the running
	// totals. A store write failure surfaces as a job error so the cap is
	// never silently under-counted.
	if h.quota != nil {
		if rerr := h.quota.Record(ctx, gitSHA, resp.Usage.Total()); rerr != nil {
			return fmt.Errorf("review.Handle: record token usage for %q: %w", kind, rerr)
		}
	}

	// Parse validates the response shape, then each parsed ReviewFinding is
	// converted to a domain.Finding and persisted. A Save failure surfaces as
	// a job error so the poller retries: the job is not "done" until its
	// findings persist.
	parsed, err := prompt.Parse(resp.Text)
	if err != nil {
		return fmt.Errorf("review.Handle: parse response for %q: %w", kind, err)
	}
	for _, rf := range parsed {
		f, ferr := toDomainFinding(rf, in.RepoID, in.Branch, in.FilePath)
		if ferr != nil {
			return fmt.Errorf("review.Handle: convert finding for %q: %w", kind, ferr)
		}
		if serr := h.findings.Save(ctx, f); serr != nil {
			return fmt.Errorf("review.Handle: save finding for %q: %w", kind, serr)
		}
	}
	return nil
}

// Compile-time check: *Handler satisfies ports.WorkHandler.
var _ ports.WorkHandler = (*Handler)(nil)

// atomicString guards the once-per-day audit gate. compareAndSwap returns true
// exactly once for a given value until it is overwritten by a different value
// (a new local day) or cleared by reset.
type atomicString struct {
	mu  sync.Mutex
	val string
}

// compareAndSwap stores v and returns true when v differs from the stored
// value (i.e. this is the first call for v); otherwise it returns false.
func (a *atomicString) compareAndSwap(v string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.val == v {
		return false
	}
	a.val = v
	return true
}

// reset clears the stored value when it still equals v, so the next
// compareAndSwap(v) succeeds again (used to retry a failed audit write).
func (a *atomicString) reset(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.val == v {
		a.val = ""
	}
}
