// Package embedder contains the long-running goroutine that drains
// node_embedding_refs(state='pending'), computes embeddings via the
// EmbeddingProvider, persists the bytes to node_embeddings, and upserts
// them into the VectorStorage so they become searchable in the same tick.
//
// Scope (m3.02.1): correctness of the loop. Out of scope:
//   - rate limiting (m3.02.2),
//   - retry policy (m3.02.3),
//   - content_hash dedup before calling Embed (m3.02.4).
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

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// DefaultBatchSize is the maximum number of refs drained per tick (M3 §m3.02).
const DefaultBatchSize = 32

// DefaultInterval is the poll cadence — matches the post_promotion_queue
// Poller default so back-pressure characteristics line up.
const DefaultInterval = 250 * time.Millisecond

// Worker drains pending node_embedding_refs, embeds them, and upserts
// vectors into VectorStorage. It owns no state beyond what's needed to
// service one tick; all durability lives in the SQLite refs table.
type Worker struct {
	refs     ports.EmbeddingRefRepo
	embedder ports.EmbeddingProvider
	vectors  ports.VectorStorage
	metrics  *observability.Metrics

	batchSize int
	interval  time.Duration

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
		refs:      refs,
		embedder:  embedder,
		vectors:   vectors,
		batchSize: DefaultBatchSize,
		interval:  DefaultInterval,
		done:      make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
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

	for _, ref := range pending {
		if ctx.Err() != nil {
			return
		}
		vec, err := w.embedder.Embed(ctx, ref.Text)
		if err != nil {
			// Per-row failure: leave the row in 'pending'. Retry policy
			// lives in m3.02.3; siblings still proceed.
			continue
		}
		if len(vec) == 0 {
			continue
		}

		contentHash := hashEmbedding(modelID, vec)
		blob := encodeFloat32LE(vec)

		if err := w.refs.MarkReady(ctx, ref.NodeID, contentHash, modelID, len(vec), blob, now); err != nil {
			// Persistence failure for this row — skip the vector upsert
			// for it so we don't surface a vector hit for a row the SQL
			// side won't acknowledge.
			continue
		}

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

// hashEmbedding returns a stable content_hash for (model, vector). The hash
// is purely a deduplication / PK device for node_embeddings; collisions are
// astronomically unlikely with SHA-256.
func hashEmbedding(modelID string, vec []float32) string {
	h := sha256.New()
	_, _ = h.Write([]byte(modelID))
	_, _ = h.Write([]byte{0})
	buf := make([]byte, 4)
	for _, f := range vec {
		binary.LittleEndian.PutUint32(buf, math.Float32bits(f))
		_, _ = h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil))
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
