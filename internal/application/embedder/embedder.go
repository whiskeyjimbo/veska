// Package embedder contains the long-running goroutine that drains
// node_embedding_refs(state='pending'), computes embeddings via the
// EmbeddingProvider, persists the bytes to node_embeddings, and upserts
// them into the VectorStorage so they become searchable in the same tick.
// Scope (m3.02.1): correctness of the loop. m3.02.2 added rate limiting,
// m3.02.3 added retry policy, m3.02.4 added content-addressed dedup that
// skips the EmbeddingProvider.Embed call when the (modelID, embed_text)
// hash already has a row in node_embeddings.
// Lifecycle mirrors the post_promotion_queue Poller: Start launches one
// background goroutine; passing a cancelled context (or calling Stop)
// terminates it cleanly; Wait blocks until exit.
package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// DefaultBatchSize is the maximum number of refs drained per tick (M3 §m3.02).
const DefaultBatchSize = 32

// DefaultInterval is the poll cadence - matches the post_promotion_queue
// Poller default so back-pressure characteristics line up.
const DefaultInterval = 250 * time.Millisecond

// DefaultRatePerSec caps the Embed-call rate when no WithRatePerSec is
// supplied. Chosen to be a conservative default that keeps a local Ollama
// instance from being hammered while still allowing reasonable throughput.
// Set via WithRatePerSec(0) to disable the limiter entirely.
const DefaultRatePerSec = 10.0

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
	MarkReady(ctx context.Context, nodeID, contentHash, modelID string, dim int, embedding []byte, at time.Time) error
	MarkAttemptFailed(ctx context.Context, nodeID string, maxAttempts int) error
	Reuse(ctx context.Context, nodeID, contentHash string, at time.Time) error
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

	// rateExplicit records whether WithRatePerSec was ever called. We need
	// this so a caller can pass WithRatePerSec(0) to disable the limiter,
	// while an unset option falls through to DefaultRatePerSec.
	rateExplicit bool
	ratePerSec   float64
	limiter      *rate.Limiter

	// pauser, when non-nil and returning true, causes tick to skip its
	// FetchPending+Embed pass. The poll loop still runs at interval so
	// the worker resumes promptly when the gate clears. Used by the
	// daemon to hold the embedder off the Write pool while the
	// resync path is committing on Write ( - closes the
	// race 's queue-poller pause only partially fixed).
	pauser func() bool

	mu        sync.Mutex
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// Option configures a Worker.
type Option func(*Worker)

// WithBatchSize overrides the per-tick batch size (default 32).
// Values <= 0 are ignored.
func WithBatchSize(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.batchSize = n
		}
	}
}

// WithInterval overrides the poll cadence (default 250ms).
// Values <= 0 are ignored.
func WithInterval(d time.Duration) Option {
	return func(w *Worker) {
		if d > 0 {
			w.interval = d
		}
	}
}

// WithRatePerSec installs a token-bucket rate limit on Embed calls. The
// limiter is checked once per row before invoking EmbeddingProvider.Embed.
// A value of 0 (or negative) disables the limiter entirely - useful for
// tests and for backends that handle their own throttling. If the option
// is not supplied, the worker uses DefaultRatePerSec.
// The bucket size is fixed at 1: the limiter smooths load rather than
// allowing bursts. A cold worker issues its first Embed immediately and
// then waits 1/r seconds between subsequent calls.
func WithRatePerSec(r float64) Option {
	return func(w *Worker) {
		w.rateExplicit = true
		w.ratePerSec = r
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

// WithPauser installs a predicate the worker consults at every tick.
// When it returns true the tick is a no-op - no FetchPending, no
// Embed, no writes. Used by the daemon to hold the embedder off the
// db while resync / cold-scan is committing.
func WithPauser(p func() bool) Option {
	return func(w *Worker) { w.pauser = p }
}

// WithMetrics installs a Metrics struct so the worker can publish
// veska_embed_queue_depth on every tick. When nil the gauge update is
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
// at first tick.
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
		done:        make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	// Resolve the limiter after options have been applied. If the caller
	// never invoked WithRatePerSec, fall back to the default. A zero or
	// negative rate means "no limiter installed" - the per-row gate is a
	// nil-check away.
	effective := w.ratePerSec
	if !w.rateExplicit {
		effective = DefaultRatePerSec
	}
	if effective > 0 {
		w.limiter = rate.NewLimiter(rate.Limit(effective), 1)
	}
	return w, nil
}

// Start launches the poll loop in a new goroutine and returns immediately.
// Subsequent calls are no-ops (the worker may only be started once).
// The provided ctx is the parent for the worker's lifetime; cancelling it
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

// run is the poll loop body. It exits when ctx is cancelled.
func (w *Worker) run(ctx context.Context) {
	defer close(w.done)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		w.tick(ctx)

		// Reset for next tick. Honour cancellation immediately rather
		// than waiting a full interval.
		if ctx.Err() != nil {
			return
		}
		timer.Reset(w.interval)
	}
}

// tick performs one drain attempt. Errors from a single row are isolated
// to that row; sibling rows in the same batch still succeed. The depth
// gauge is updated once per tick whether or not work was processed.
func (w *Worker) tick(ctx context.Context) {
	if w.pauser != nil && w.pauser() {
		// While paused we deliberately do NOT touch the refs table - not
		// even the count probe - so the Write pool stays idle and
		// can't race the Write promotion tx into SQLITE_BUSY
		// The next tick re-checks; the daemon clears the
		// pause when resync finishes.
		return
	}
	if depth, err := w.refs.CountPending(ctx); err == nil && w.metrics != nil && w.metrics.EmbedQueueDepth != nil {
		w.metrics.EmbedQueueDepth.Set(float64(depth))
	}

	pending, err := w.refs.FetchPending(ctx, w.batchSize)
	if err != nil || len(pending) == 0 {
		return
	}

	// Group successful embeddings by (repo_id, branch) so they can be
	// upserted into VectorStorage in batches keyed correctly. The vector
	// port is per-(repo,branch) - a single upsert call can't span them.
	type vecKey struct{ repo, branch string }
	vecBatches := make(map[vecKey][]domain.EmbeddingRow)

	now := time.Now()
	modelID := w.embedder.ModelID()

	// inFlight maps content_hash → vector for hashes produced by Embed in
	// THIS tick. It services intra-tick dedup: when two siblings in the same
	// batch project to the same key, the first calls Embed and writes a row,
	// and subsequent siblings reuse the bytes without a second provider
	// call. SQLite ON CONFLICT DO NOTHING would handle the row collision
	// either way, but the in-memory map avoids the redundant Embed itself.
	inFlight := make(map[string][]float32)

	// Pass 1: classify each ref. Fast paths (in-flight cache, prior-tick
	// lookup) finish immediately. Cache-misses are queued for a single
	// batched Embed call below so cobra cold-scan doesn't
	// pay 67 sequential Ollama roundtrips.
	type pendingRef struct {
		ref         ports.PendingEmbedRef
		contentHash string
	}
	var needsEmbed []pendingRef
	enqueueVec := func(ref ports.PendingEmbedRef, contentHash string, vec []float32) {
		key := vecKey{repo: ref.RepoID, branch: ref.Branch}
		vecBatches[key] = append(vecBatches[key], domain.EmbeddingRow{
			NodeID:      ref.NodeID,
			ContentHash: contentHash,
			ModelID:     modelID,
			Vector:      vec,
		})
	}

	for _, ref := range pending {
		if ctx.Err() != nil {
			return
		}

		contentHash := hashEmbedText(modelID, ref.Text)

		// Fast path 1: this tick already embedded this exact key.
		if vec, ok := inFlight[contentHash]; ok {
			if err := w.refs.Reuse(ctx, ref.NodeID, contentHash, now); err != nil {
				continue
			}
			if w.metrics != nil && w.metrics.EmbedDedupHits != nil {
				w.metrics.EmbedDedupHits.Inc()
			}
			enqueueVec(ref, contentHash, vec)
			continue
		}

		// Fast path 2: a prior tick already embedded this key - the bytes
		// are in node_embeddings.
		if blob, dim, found, err := w.refs.LookupExisting(ctx, contentHash); err == nil && found {
			vec := veccodec.DecodeFloat32LE(blob, dim)
			if err := w.refs.Reuse(ctx, ref.NodeID, contentHash, now); err != nil {
				continue
			}
			if w.metrics != nil && w.metrics.EmbedDedupHits != nil {
				w.metrics.EmbedDedupHits.Inc()
			}
			inFlight[contentHash] = vec
			enqueueVec(ref, contentHash, vec)
			continue
		}

		needsEmbed = append(needsEmbed, pendingRef{ref: ref, contentHash: contentHash})
	}

	// Pass 2: deduplicate by content_hash within needsEmbed so two refs
	// with identical text only cost one Embed call. Order matters for
	// EmbedBatch's contract - preserve first-seen order.
	uniqueByHash := make(map[string]int, len(needsEmbed))
	var uniqueTexts []string
	for _, p := range needsEmbed {
		if _, seen := uniqueByHash[p.contentHash]; seen {
			continue
		}
		uniqueByHash[p.contentHash] = len(uniqueTexts)
		uniqueTexts = append(uniqueTexts, p.ref.Text)
	}

	// Pass 3: call Embed (batch when the provider supports it, otherwise
	// loop). Rate limiter counts each unique text - preserves the
	// per-second cap callers expect (a batch of 32 unique texts is
	// still 32 'requests' under the limiter's accounting).
	uniqueVecs := make([][]float32, len(uniqueTexts))
	failedTexts := make(map[int]bool)
	usedBatch := false
	if batchProv, ok := w.embedder.(ports.BatchEmbeddingProvider); ok && len(uniqueTexts) > 1 {
		// A batch is ONE network roundtrip → one rate-limit event.
		// Accounting for it as N would make WaitN(32) hang for ~3s on
		// a 10rps cap with burst=1, defeating the batch optimization.
		// The serial fallback below still does per-text Wait so the
		// per-second cap still backstops a misbehaving embedder.
		if w.limiter != nil {
			if err := w.limiter.Wait(ctx); err != nil {
				return
			}
		}
		vecs, err := batchProv.EmbedBatch(ctx, uniqueTexts)
		switch {
		case err == nil:
			copy(uniqueVecs, vecs)
			usedBatch = true
		case errors.Is(err, ports.ErrBatchEmbedNotSupported):
			// Wrapped provider didn't actually support batch - fall
			// through to the serial path below. usedBatch stays false.
		default:
			// Real batch failure (ErrEmbedderUnreachable, etc.) - mark
			// every unique text as failed; MarkAttemptFailed below will
			// bump attempts and (eventually) flip to state='failed'.
			for i := range uniqueTexts {
				failedTexts[i] = true
			}
			usedBatch = true
		}
	}
	if !usedBatch {
		for i, text := range uniqueTexts {
			if ctx.Err() != nil {
				return
			}
			if w.limiter != nil {
				if err := w.limiter.Wait(ctx); err != nil {
					return
				}
			}
			vec, err := w.embedder.Embed(ctx, text)
			if err != nil {
				failedTexts[i] = true
				continue
			}
			uniqueVecs[i] = vec
		}
	}

	// Pass 4: persist + distribute. Each pending ref looks up the unique
	// slot for its content_hash and either persists the new vector or
	// marks attempt-failed.
	for _, p := range needsEmbed {
		slot := uniqueByHash[p.contentHash]
		if failedTexts[slot] {
			_ = w.refs.MarkAttemptFailed(ctx, p.ref.NodeID, w.maxAttempts)
			continue
		}
		vec := uniqueVecs[slot]
		if len(vec) == 0 {
			continue
		}
		// First time we see this content_hash, do the L2 normalize +
		// node_embeddings INSERT. Siblings reuse via the inFlight cache.
		existing, cached := inFlight[p.contentHash]
		if !cached {
			l2Normalize(vec)
			blob := veccodec.EncodeFloat32LE(vec)
			if err := w.refs.MarkReady(ctx, p.ref.NodeID, p.contentHash, modelID, len(vec), blob, now); err != nil {
				continue
			}
			inFlight[p.contentHash] = vec
			existing = vec
		} else {
			if err := w.refs.Reuse(ctx, p.ref.NodeID, p.contentHash, now); err != nil {
				continue
			}
			if w.metrics != nil && w.metrics.EmbedDedupHits != nil {
				w.metrics.EmbedDedupHits.Inc()
			}
		}
		enqueueVec(p.ref, p.contentHash, existing)
	}

	for k, batch := range vecBatches {
		// VectorStorage errors are non-fatal; the durable record is the
		// SQL ref row already marked 'ready'. A future retry / reindex
		// path can rebuild the vector store from node_embeddings bytes.
		_ = w.vectors.UpsertEmbeddings(ctx, k.repo, k.branch, batch)
	}
}

// hashEmbedText returns a content_hash keyed on the EMBED INPUT - the model
// identifier and the deterministic embed_text projection. Two refs that hash
// to the same value are guaranteed to produce the same vector under this
// model, so the worker can dedup before calling EmbeddingProvider.Embed.
// The model is mixed into the hash so swapping models invalidates dedup
// against prior bytes: a re-embed under a new model produces fresh bytes
// rather than aliasing onto the previous model's vector.
func hashEmbedText(modelID, embedText string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(modelID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(embedText))
	return hex.EncodeToString(h.Sum(nil))
}

// l2Normalize scales vec in place to unit L2 norm. A zero vector is left
// unchanged. Keeping every stored embedding unit-length is what makes the
// VectorStorage score 1/(1+L2dist) - and thus the auto-link threshold
// behave as documented regardless of the embedding model's native scale.
func l2Normalize(vec []float32) {
	var sq float64
	for _, f := range vec {
		sq += float64(f) * float64(f)
	}
	if sq == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sq))
	for i := range vec {
		vec[i] *= inv
	}
}
