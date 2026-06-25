// SPDX-License-Identifier: AGPL-3.0-only

// Package embedder contains the long-running goroutine that drains
// node_embedding_refs(state='pending'), computes embeddings via the
// EmbeddingProvider, persists the bytes to node_embeddings, and upserts
// them into the VectorStorage so they become searchable in the same tick.
// Scope (m3.02.1): correctness of the loop. m3.02.3 added retry policy,
// m3.02.4 added content-addressed dedup that skips the EmbeddingProvider.Embed
// call when the (modelID, embed_text) hash already has a row in
// node_embeddings. A later change replaced the fixed token-bucket rate limiter
// (which was dominated by - and effectively inert behind - the poll interval)
// with a greedy drain bounded by a Governor: the loop runs passes back-to-back
// while the queue stays full, and embeds up to Governor.Limit() batches
// concurrently. All SQL stays on the drain goroutine; only the pure embed call
// is offloaded, so concurrency carries no write-contention risk.
// Lifecycle mirrors the post_promotion_queue Poller: Start launches one
// background goroutine; passing a canceled context (or calling Stop)
// terminates it cleanly; Wait blocks until exit.
package embedder

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
	"github.com/whiskeyjimbo/veska/internal/platform/pollloop"
)

// DefaultBatchSize is the maximum number of refs drained per batch (M3 §m3.02).
const DefaultBatchSize = 32

// DefaultInterval is the IDLE poll cadence - how long the worker sleeps after a
// drain pass comes up short (queue drained). While the queue keeps yielding
// full batches the worker drains greedily without waiting; the interval only
// governs how often it re-checks an empty queue. Matches the
// post_promotion_queue Poller default so back-pressure characteristics line up.
const DefaultInterval = 250 * time.Millisecond

// depthGaugeInterval bounds how often the worker runs the O(pending)
// CountPending behind the EmbedQueueDepth gauge during a greedy drain.
const depthGaugeInterval = time.Second

// DefaultMaxAttempts is the per-row retry budget. After this many Embed
// failures on the same row, MarkAttemptFailed flips the row to
// state='failed' and FetchPending stops returning it. Overridable via
// WithMaxAttempts.
const DefaultMaxAttempts = 3

// EmbedRefQueue is the consumer-owned slice of ports.EmbeddingRefRepo that the
// Worker actually uses to drain and resolve the embedding queue. It omits the
// two methods the Worker never calls (ContentHashForNode, CountByState - those
// belong to autolink and the doctor count probes, which declare their own
// narrow interfaces). Following the same ISP convention as those consumers
// keeps the Worker's dependency honest; the single sqlite.EmbeddingRefsRepo
// satisfies this and the full ports.EmbeddingRefRepo alike.
type EmbedRefQueue interface {
	FetchPending(ctx context.Context, limit int) ([]ports.PendingEmbedRef, error)
	CountPending(ctx context.Context) (int, error)
	LookupExisting(ctx context.Context, contentHash string) (embedding []byte, dim int, found bool, err error)
	// LookupExistingBatch resolves many content hashes in one query, replacing the
	// per-row LookupExisting N+1 in classify - the dominant cost once embed compute
	// is parallel. Missing hashes are absent from the map.
	LookupExistingBatch(ctx context.Context, hashes []string) (map[string]ports.ExistingEmbedding, error)
	// ApplyEmbedBatch flushes a whole drain batch's writes in one transaction:
	// unique embedding inserts, ready-ref flips (fresh + dedup hits), and
	// attempt bumps for failures. Replaces the per-row MarkReady/Reuse/
	// MarkAttemptFailed calls that each opened their own transaction.
	ApplyEmbedBatch(ctx context.Context, inserts []ports.EmbedInsert, ready []ports.EmbedReadyRef, failed []string, modelID string, maxAttempts int, at time.Time) error
}

// Worker drains pending node_embedding_refs, embeds them, and upserts
// vectors into VectorStorage. It owns no state beyond what's needed to
// service one tick; all durability lives in the SQLite refs table.
type Worker struct {
	refs     EmbedRefQueue
	embedder ports.EmbeddingProvider
	vectors  ports.VectorStorage
	metrics  *observability.Metrics

	batchSize   int
	interval    time.Duration
	maxAttempts int

	// governor bounds how many embed calls may be in flight and absorbs
	// per-call Outcome feedback (latency, 429 Retry-After). The default is a
	// fixed limit of 1, which reproduces the prior serial drain for a local
	// Ollama instance; hosted-API providers supply an adaptive governor.
	governor Governor

	// pauser, when non-nil and returning true, causes a drain pass to skip
	// its FetchPending+Embed work. The poll loop still runs at interval so
	// the worker resumes promptly when the gate clears. Used by the
	// daemon to hold the embedder off the Write pool while the
	// resync path is committing on Write ( - closes the
	// race 's queue-poller pause only partially fixed).
	pauser func() bool

	// depthGaugeAt time-gates the EmbedQueueDepth gauge update. CountPending is
	// O(pending) and the drain loops passes back-to-back, so counting every pass
	// is hot-loop waste; the gauge only needs second-resolution freshness.
	// Touched only on the single drain goroutine, so it needs no lock.
	depthGaugeAt time.Time

	mu        sync.Mutex
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// Option configures a Worker.
type Option func(*Worker)

// WithBatchSize overrides the per-batch size (default 32).
// Values <= 0 are ignored.
func WithBatchSize(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.batchSize = n
		}
	}
}

// WithInterval overrides the idle poll cadence (default 250ms).
// Values <= 0 are ignored.
func WithInterval(d time.Duration) Option {
	return func(w *Worker) {
		if d > 0 {
			w.interval = d
		}
	}
}

// WithGovernor installs the concurrency Governor that bounds in-flight embed
// calls. When unset the worker uses NewFixedGovernor(1) - a serial drain that
// matches a single local Ollama instance (which serializes embeds internally,
// so concurrency past 1 buys nothing). Hosted-API providers pass an adaptive
// governor sized to their RPM/TPM quota.
func WithGovernor(g Governor) Option {
	return func(w *Worker) {
		if g != nil {
			w.governor = g
		}
	}
}

// WithMaxAttempts overrides the per-row retry budget (default 3).
// After this many consecutive Embed failures on the same row, the row is
// flipped to state='failed' and excluded from future FetchPending results.
// Values <= 0 are ignored - use 1 to fail rows after a single error.
func WithMaxAttempts(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.maxAttempts = n
		}
	}
}

// WithPauser installs a predicate the worker consults at every pass.
// When it returns true the pass is a no-op - no FetchPending, no
// Embed, no writes. Used by the daemon to hold the embedder off the
// db while resync / cold-scan is committing.
func WithPauser(p func() bool) Option {
	return func(w *Worker) { w.pauser = p }
}

// WithMetrics installs a Metrics struct so the worker can publish
// veska_embed_queue_depth on every pass. When nil the gauge update is
// silently skipped - the worker still functions.
func WithMetrics(m *observability.Metrics) Option {
	return func(w *Worker) { w.metrics = m }
}

// ErrMissingDependency is returned by NewWorker when a required collaborator
// (refs, embedder, or vectors) is nil. It is errors.Is-matchable so callers
// can distinguish a wiring fault from a runtime failure.
var ErrMissingDependency = errors.New("embedder: missing required dependency")

// NewWorker constructs a Worker. Dependencies are required: all three of
// refs, embedder, and vectors must be non-nil. A nil dependency yields an
// error wrapping ErrMissingDependency and a nil *Worker - surfacing the
// wiring fault at construction time rather than crashing inside a goroutine
// at first pass.
func NewWorker(
	refs EmbedRefQueue,
	embedder ports.EmbeddingProvider,
	vectors ports.VectorStorage,
	opts ...Option,
) (*Worker, error) {
	if refs == nil {
		return nil, fmt.Errorf("embedder.NewWorker: refs is nil: %w", ErrMissingDependency)
	}
	if embedder == nil {
		return nil, fmt.Errorf("embedder.NewWorker: embedder is nil: %w", ErrMissingDependency)
	}
	if vectors == nil {
		return nil, fmt.Errorf("embedder.NewWorker: vectors is nil: %w", ErrMissingDependency)
	}
	w := &Worker{
		refs:        refs,
		embedder:    embedder,
		vectors:     vectors,
		batchSize:   DefaultBatchSize,
		interval:    DefaultInterval,
		maxAttempts: DefaultMaxAttempts,
		governor:    NewFixedGovernor(1),
		done:        make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	return w, nil
}

// Start launches the poll loop in a new goroutine and returns immediately.
// Subsequent calls are no-ops (the worker may only be started once).
// The provided ctx is the parent for the worker's lifetime; canceling it
// stops the loop. Stop and Wait are also available for callers that want
// explicit lifecycle control without owning the ctx.
func (w *Worker) Start(ctx context.Context) {
	w.startOnce.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		w.mu.Lock()
		w.cancel = cancel
		w.mu.Unlock()
		go w.run(runCtx)
	})
}

// Stop cancels the worker's context and waits for the goroutine to exit.
// Safe to call multiple times. If Start was never called, Stop is a no-op:
// there's no goroutine to wait on, and the done channel is left open so a
// later call to Start still works.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel == nil {
		// Start was never called; nothing to stop.
		return
	}
	w.stopOnce.Do(func() { cancel() })
	<-w.done
}

// Wait blocks until the worker's goroutine has exited. Used by callers
// who cancel the parent ctx and want a synchronous shutdown barrier
// without going through Stop.
func (w *Worker) Wait() { <-w.done }

// run is the poll loop body. It exits when ctx is canceled.
//
// Greedy drain: drainPass runs back-to-back while the queue keeps yielding
// full loads, falling back to the idle interval only once a pass comes up
// short (drainPass also returns false while paused). The shared pollloop.Run
// encodes that "don't sleep a full interval between batches" rule - see its
// doc for why.
func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	pollloop.Run(ctx, w.interval, w.drainPass)
}

// drainPass performs one drain attempt and reports whether it drained a full
// load (every batch came back at batchSize and the governor's slots were
// filled), which signals the caller to loop again immediately rather than
// idle. Errors from a single row are isolated to that row; sibling rows in
// the same batch still succeed.
//
// Three stages, in order:
//   - classify (serial, reads + dedup-Reuse writes): resolve fast-path refs
//     and collect what needs embedding.
//   - embed (concurrent, governed, NO SQL): the pure provider calls.
//   - persist (serial, writes): MarkReady/Reuse/MarkAttemptFailed + vector
//     upsert.
//
// Keeping every SQL touch on this goroutine is deliberate: it lets the embed
// stage run concurrently without any write-contention or ordering hazard.
func (w *Worker) drainPass(ctx context.Context) bool {
	if w.pauser != nil && w.pauser() {
		// While paused we deliberately do NOT touch the refs table - not
		// even the count probe - so the Write pool stays idle and
		// can't race the Write promotion tx into SQLITE_BUSY.
		// The next pass re-checks; the daemon clears the
		// pause when resync finishes.
		return false
	}
	// Publish the queue-depth gauge at most once per second: the CountPending it
	// needs is O(pending) and a greedy drain runs many passes per second.
	if w.metrics != nil && w.metrics.EmbedQueueDepth != nil {
		if now := time.Now(); now.Sub(w.depthGaugeAt) >= depthGaugeInterval {
			w.depthGaugeAt = now
			if depth, err := w.refs.CountPending(ctx); err == nil {
				w.metrics.EmbedQueueDepth.Set(float64(depth))
			}
		}
	}

	limit := max(w.governor.Limit(), 1)
	if w.metrics != nil && w.metrics.EmbedConcurrencyLimit != nil {
		w.metrics.EmbedConcurrencyLimit.Set(float64(limit))
	}

	// Fetch this pass's rows in ONE query, then split into up to `limit`
	// per-batch jobs for the governed embed stage. FetchPending does not claim
	// rows - they stay 'pending' until the persist stage runs - so calling it
	// once per batch would hand every batch the same top rows, double-embedding
	// and double-writing at limit>1. A single fetch of limit*batchSize keeps
	// the batches disjoint.
	pending, err := w.refs.FetchPending(ctx, limit*w.batchSize)
	if err != nil || len(pending) == 0 {
		return false
	}
	// A full haul means the queue probably still has more - loop again
	// immediately rather than falling back to the idle interval.
	full := len(pending) == limit*w.batchSize

	jobs := make([]*batchJob, 0, limit)
	for start := 0; start < len(pending); start += w.batchSize {
		end := min(start+w.batchSize, len(pending))
		jobs = append(jobs, w.classify(ctx, pending[start:end]))
	}

	w.embedJobs(ctx, jobs)
	if ctx.Err() != nil {
		return false
	}
	for _, job := range jobs {
		w.persist(ctx, job)
	}
	return full
}
