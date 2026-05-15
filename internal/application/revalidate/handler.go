// Package revalidate implements the post-promotion sweep that closes open
// findings whose anchor symbol has drifted on disk.
//
// One queue row -> one file. The handler asks the RevalidateQuerier port for
// the set of open findings on that file whose recorded anchor_content_hash
// no longer matches the current node's content_hash, and closes each with
// closed_reason='revalidated_obsolete'. The next promotion's check runner
// (m3.01) re-emits a fresh finding IF the rule still fires on the new
// content; m3.05.2 deliberately does not run the check inline (that
// refinement lands in 5.3).
//
// Scope discipline:
//   - File-bounded: scope = (payload file path).
//   - Branch-bounded: scope = row.Branch.
//   - Repo-bounded:   scope = row.RepoID.
//   - NULL anchor_content_hash: untouched (file-anchored findings have no
//     hash to compare against).
//   - Already-closed findings: untouched (UPDATE gated on state='open').
package revalidate

import (
	"context"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// Handler implements ports.WorkHandler for WorkKindRevalidate rows.
//
// Failure semantics mirror autolink.Handler: SQL errors propagate wrapped so
// queue.Poller's retry path runs; programmer errors (wrong WorkKind) return
// a wrapped error so misrouting is loud.
type Handler struct {
	repo    ports.RevalidateQuerier
	clock   func() time.Time
	metrics *observability.Metrics
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithClock replaces the wall-clock used for the closed_at stamp. Primarily
// for tests. The default is time.Now.
func WithClock(c func() time.Time) Option {
	return func(h *Handler) {
		if c != nil {
			h.clock = c
		}
	}
}

// WithMetrics attaches a Metrics struct so the handler can increment
// veska_revalidate_closed_total per closure. Optional: a nil-metrics handler
// is fully functional and simply does not emit the counter.
func WithMetrics(m *observability.Metrics) Option {
	return func(h *Handler) {
		h.metrics = m
	}
}

// NewHandler constructs a Handler bound to the given RevalidateQuerier.
// repo is required; nil panics at construction time to mirror autolink.
func NewHandler(repo ports.RevalidateQuerier, opts ...Option) *Handler {
	if repo == nil {
		panic("revalidate.NewHandler: repo is nil")
	}
	h := &Handler{
		repo:  repo,
		clock: time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Handle processes a single ports.WorkRow of kind WorkKindRevalidate.
//
// Behaviour:
//   - Wrong kind: wrapped error (routing bug).
//   - Empty payload: nil (no file => nothing to sweep).
//   - SQL error from either port method: wrapped error so the Poller retries.
//   - Per stale finding: one CloseAsRevalidatedObsolete call + one
//     RevalidateClosed counter increment.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindRevalidate {
		return fmt.Errorf("revalidate.Handle: unexpected kind %q", row.Kind)
	}
	filePath := row.Payload
	if filePath == "" {
		return nil
	}

	stale, err := h.repo.StaleFindingsForFile(ctx, row.RepoID, row.Branch, filePath)
	if err != nil {
		return fmt.Errorf("revalidate.Handle: stale findings for %q: %w", filePath, err)
	}
	if len(stale) == 0 {
		return nil
	}

	closedAt := h.clock().UnixMilli()
	for _, s := range stale {
		if err := h.repo.CloseAsRevalidatedObsolete(ctx, row.RepoID, row.Branch, s.FindingID, closedAt); err != nil {
			return fmt.Errorf("revalidate.Handle: close %q: %w", s.FindingID, err)
		}
		if h.metrics != nil && h.metrics.RevalidateClosed != nil {
			h.metrics.RevalidateClosed.Inc()
		}
	}
	return nil
}

// Compile-time check: *Handler satisfies ports.WorkHandler.
var _ ports.WorkHandler = (*Handler)(nil)
