// Package embedder contains the long-running goroutine that drains
// node_embedding_refs(state='pending'), computes embeddings via the
// EmbeddingProvider, persists the bytes to node_embeddings, and upserts
// them into the VectorStorage so they become searchable in the same tick.
//
// Scope (m3.02.1): correctness of the loop. m3.02.2 added rate limiting,
// m3.02.3 added retry policy, m3.02.4 added content-addressed dedup that
// skips the EmbeddingProvider.Embed call when the (modelID, embed_text)
// hash already has a row in node_embeddings.
//
// Lifecycle mirrors the post_promotion_queue Poller: Start launches one
// background goroutine; passing a cancelled context (or calling Stop)
// terminates it cleanly; Wait blocks until exit.
package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// DefaultBatchSize is the maximum number of refs drained per tick (M3 §m3.02).
const DefaultBatchSize = 32

// DefaultInterval is the poll cadence — matches the post_promotion_queue
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

// Worker drains pending node_embedding_refs, embeds them, and upserts
// vectors into VectorStorage. It owns no state beyond what's needed to
// service one tick; all durability lives in the SQLite refs table.
type Worker struct {
	refs     ports.EmbeddingRefRepo
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
//
// A value of 0 (or negative) disables the limiter entirely — useful for
// tests and for backends that handle their own throttling. If the option
// is not supplied, the worker uses DefaultRatePerSec.
//
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
// Values <= 0 are ignored — use 1 to fail rows after a single error.
func WithMaxAttempts(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.maxAttempts = n
		}
	}
}

// WithMetrics installs a Metrics struct so the worker can publish
// veska_embed_queue_depth on every tick. When nil the gauge update is
// silently skipped — the worker still functions.
func WithMetrics(m *observability.Metrics) Option {
	return func(w *Worker) { w.metrics = m }
}

// NewWorker constructs a Worker. Dependencies are required: all three of
// refs, embedder, and vectors must be non-nil — a nil dependency is a
// programmer error and is reported by panicking at construction time
// (rather than crashing inside a goroutine at first tick).
func NewWorker(
	refs ports.EmbeddingRefRepo,
	embedder ports.EmbeddingProvider,
	vectors ports.VectorStorage,
	opts ...Option,
) *Worker {
	if refs == nil {
		panic("embedder.NewWorker: refs is nil")
	}
	if embedder == nil {
		panic("embedder.NewWorker: embedder is nil")
	}
	if vectors == nil {
		panic("embedder.NewWorker: vectors is nil")
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
	// negative rate means "no limiter installed" — the per-row gate is a
	// nil-check away.
	effective := w.ratePerSec
	if !w.rateExplicit {
		effective = DefaultRatePerSec
	}
	if effective > 0 {
		w.limiter = rate.NewLimiter(rate.Limit(effective), 1)
	}
	return w
}

// Start launches the poll loop in a new goroutine and returns immediately.
// Subsequent calls are no-ops (the worker may only be started once).
//
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
	if depth, err := w.refs.CountPending(ctx); err == nil && w.metrics != nil && w.metrics.EmbedQueueDepth != nil {
		w.metrics.EmbedQueueDepth.Set(float64(depth))
	}

	pending, err := w.refs.FetchPending(ctx, w.batchSize)
	if err != nil || len(pending) == 0 {
		return
	}

	// Group successful embeddings by (repo_id, branch) so they can be
	// upserted into VectorStorage in batches keyed correctly. The vector
	// port is per-(repo,branch) — a single upsert call can't span them.
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
			key := vecKey{repo: ref.RepoID, branch: ref.Branch}
			vecBatches[key] = append(vecBatches[key], domain.EmbeddingRow{
				NodeID:      ref.NodeID,
				ContentHash: contentHash,
				ModelID:     modelID,
				Vector:      vec,
			})
			continue
		}

		// Fast path 2: a prior tick already embedded this key — the bytes
		// are in node_embeddings.
		if blob, dim, found, err := w.refs.LookupExisting(ctx, contentHash); err == nil && found {
			vec := decodeFloat32LE(blob, dim)
			if err := w.refs.Reuse(ctx, ref.NodeID, contentHash, now); err != nil {
				continue
			}
			if w.metrics != nil && w.metrics.EmbedDedupHits != nil {
				w.metrics.EmbedDedupHits.Inc()
			}
			inFlight[contentHash] = vec
			key := vecKey{repo: ref.RepoID, branch: ref.Branch}
			vecBatches[key] = append(vecBatches[key], domain.EmbeddingRow{
				NodeID:      ref.NodeID,
				ContentHash: contentHash,
				ModelID:     modelID,
				Vector:      vec,
			})
			continue
		}

		// Miss — rate-limit and call Embed. limiter.Wait honours ctx — when
		// ctx is cancelled it returns ctx.Err() and we unwind the tick.
		if w.limiter != nil {
			if err := w.limiter.Wait(ctx); err != nil {
				return
			}
		}
		vec, err := w.embedder.Embed(ctx, ref.Text)
		if err != nil {
			// Per-row failure: bump attempts and (if budget exhausted)
			// flip the row to state='failed' so FetchPending stops
			// returning it. Siblings in this batch still proceed.
			_ = w.refs.MarkAttemptFailed(ctx, ref.NodeID, w.maxAttempts)
			continue
		}
		if len(vec) == 0 {
			continue
		}
		// L2-normalize before any persistence. Embedding models such as
		// nomic-embed-text return vectors with norm far from 1.0; the
		// VectorStorage score (1/(1+L2dist)) and the auto-link threshold
		// only behave as documented for unit vectors. Normalizing here —
		// the single point where a fresh vector enters the system —
		// covers node_embeddings, the in-flight cache, and the
		// VectorStorage upsert in one place.
		l2Normalize(vec)

		blob := encodeFloat32LE(vec)
		if err := w.refs.MarkReady(ctx, ref.NodeID, contentHash, modelID, len(vec), blob, now); err != nil {
			// Persistence failure for this row — skip the vector upsert
			// for it so we don't surface a vector hit for a row the SQL
			// side won't acknowledge.
			continue
		}
		inFlight[contentHash] = vec

		key := vecKey{repo: ref.RepoID, branch: ref.Branch}
		vecBatches[key] = append(vecBatches[key], domain.EmbeddingRow{
			NodeID:      ref.NodeID,
			ContentHash: contentHash,
			ModelID:     modelID,
			Vector:      vec,
		})
	}

	for k, batch := range vecBatches {
		// VectorStorage errors are non-fatal; the durable record is the
		// SQL ref row already marked 'ready'. A future retry / reindex
		// path can rebuild the vector store from node_embeddings bytes.
		_ = w.vectors.UpsertEmbeddings(ctx, k.repo, k.branch, batch)
	}
}

// hashEmbedText returns a content_hash keyed on the EMBED INPUT — the model
// identifier and the deterministic embed_text projection. Two refs that hash
// to the same value are guaranteed to produce the same vector under this
// model, so the worker can dedup before calling EmbeddingProvider.Embed.
//
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
// VectorStorage score 1/(1+L2dist) — and thus the auto-link threshold —
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

// encodeFloat32LE returns the little-endian byte representation of vec.
// Stored in node_embeddings.embedding as a BLOB; readers reverse this.
func encodeFloat32LE(vec []float32) []byte {
	out := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// decodeFloat32LE reverses encodeFloat32LE. dim is the expected element count;
// if the blob is short, the returned slice is truncated rather than panicking
// so a malformed row degrades to "skip this hit" at the call site.
func decodeFloat32LE(blob []byte, dim int) []float32 {
	have := len(blob) / 4
	if have < dim {
		dim = have
	}
	out := make([]float32, dim)
	for i := range dim {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4 : i*4+4]))
	}
	return out
}
