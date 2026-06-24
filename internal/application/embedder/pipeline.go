// SPDX-License-Identifier: AGPL-3.0-only

package embedder

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// vecKey groups successful embeddings by (repo_id, branch) so they can be
// upserted into VectorStorage in batches keyed correctly. The vector port is
// per-(repo,branch) - a single upsert call can't span them.
type vecKey struct{ repo, branch string }

// pendingRef pairs a fetched ref with its precomputed content hash so the
// embed and persist stages don't recompute it.
type pendingRef struct {
	ref         ports.PendingEmbedRef
	contentHash string
}

// batchJob carries one batch's state across the classify -> embed -> persist
// stages. A job is only ever touched by one stage at a time (handed off, never
// shared), so its maps need no locking even when sibling jobs embed
// concurrently. inFlight is per-job: cross-job dedup downgrades to the durable
// LookupExisting + ON CONFLICT, which keeps results correct (at most a
// redundant Embed for an identical hash split across two concurrent batches).
type batchJob struct {
	now      time.Time
	modelID  string
	inFlight map[string][]float32

	vecBatches map[vecKey][]domain.EmbeddingRow

	needsEmbed   []pendingRef
	uniqueByHash map[string]int
	uniqueTexts  []string

	uniqueVecs  [][]float32
	failedTexts map[int]bool
	// acquireErr is set when the governor could not hand out a permit (ctx
	// canceled or a Retry-After backoff outlasted the pass). The batch's
	// rows are left pending for a later retry - NOT counted as attempts.
	acquireErr error

	// Deferred writes, flushed in one ApplyEmbedBatch transaction by persist.
	// inserts holds unique embeddings to store; readyRefs the refs to flip
	// (fresh successes + classify's dedup hits); failed the node_ids to bump.
	inserts   []ports.EmbedInsert
	readyRefs []ports.EmbedReadyRef
	failed    []string
}

// classify resolves the fast-path refs (intra-pass dedup cache and
// already-embedded LookupExisting hits) and collects the rest into needsEmbed,
// deduplicated by content_hash so identical text costs one Embed call.
func (w *Worker) classify(ctx context.Context, pending []ports.PendingEmbedRef) *batchJob {
	job := &batchJob{
		now:          time.Now(),
		modelID:      w.embedder.ModelID(),
		inFlight:     make(map[string][]float32),
		vecBatches:   make(map[vecKey][]domain.EmbeddingRow),
		uniqueByHash: make(map[string]int),
	}

	// Hash every ref once, then resolve all already-stored hashes in a single
	// batch query instead of one LookupExisting per ref (the N+1 that dominates
	// the SQL-bound drain). On a lookup error, fall back to an empty map: the
	// affected refs route to needsEmbed and re-embed, which is correct (the
	// ON CONFLICT DO NOTHING insert dedups), just slower - matching the prior
	// per-row code's swallow-and-reembed behavior.
	hashes := make([]string, len(pending))
	distinct := make([]string, 0, len(pending))
	seen := make(map[string]struct{}, len(pending))
	for i, ref := range pending {
		h := hashEmbedText(job.modelID, ref.Text)
		hashes[i] = h
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			distinct = append(distinct, h)
		}
	}
	existing, err := w.refs.LookupExistingBatch(ctx, distinct)
	if err != nil {
		existing = nil
	}

	for i, ref := range pending {
		if ctx.Err() != nil {
			return job
		}

		contentHash := hashes[i]

		// Fast path 1: this pass already embedded this exact key. The ref
		// flip is deferred into the batch's ApplyEmbedBatch (no insert - the
		// embedding is already being stored by the first occurrence).
		if vec, ok := job.inFlight[contentHash]; ok {
			job.readyRefs = append(job.readyRefs, ports.EmbedReadyRef{NodeID: ref.NodeID, ContentHash: contentHash})
			w.dedupHit()
			w.enqueueVec(job, ref, contentHash, vec)
			continue
		}

		// Fast path 2: a prior pass already embedded this key - the bytes
		// are in node_embeddings (resolved by the batch lookup above). Flip
		// deferred; no insert needed.
		if e, ok := existing[contentHash]; ok {
			vec := veccodec.DecodeFloat32LE(e.Embedding, e.Dim)
			job.inFlight[contentHash] = vec
			job.readyRefs = append(job.readyRefs, ports.EmbedReadyRef{NodeID: ref.NodeID, ContentHash: contentHash})
			w.dedupHit()
			w.enqueueVec(job, ref, contentHash, vec)
			continue
		}

		job.needsEmbed = append(job.needsEmbed, pendingRef{ref: ref, contentHash: contentHash})
	}

	// Deduplicate needsEmbed by content_hash. Order matters for EmbedBatch's
	// contract - preserve first-seen order.
	for _, p := range job.needsEmbed {
		if _, seen := job.uniqueByHash[p.contentHash]; seen {
			continue
		}
		job.uniqueByHash[p.contentHash] = len(job.uniqueTexts)
		job.uniqueTexts = append(job.uniqueTexts, p.ref.Text)
	}
	return job
}

// embedJobs runs the embed stage for every job concurrently, each call gated
// by a Governor permit. This is the only stage that may run in parallel - it
// touches no SQL, so the concurrency the governor allows costs nothing in
// write contention. At the default limit of 1 the calls serialize, matching
// the prior behavior for a single local Ollama instance.
func (w *Worker) embedJobs(ctx context.Context, jobs []*batchJob) {
	var wg sync.WaitGroup
	for _, job := range jobs {
		if len(job.uniqueTexts) == 0 {
			continue // fully resolved by the fast paths
		}
		wg.Add(1)
		go func(job *batchJob) {
			defer wg.Done()

			permit, err := w.governor.Acquire(ctx)
			if err != nil {
				job.acquireErr = err
				return
			}
			start := time.Now()
			vecs, failed, callErr := w.embedTexts(ctx, job.uniqueTexts)
			lat := time.Since(start)
			permit.Release(Outcome{
				Latency:    lat,
				Err:        callErr,
				RetryAfter: RetryAfterFromErr(callErr),
			})

			job.uniqueVecs = vecs
			job.failedTexts = failed
			if w.metrics != nil && w.metrics.EmbedBatchLatency != nil {
				w.metrics.EmbedBatchLatency.Observe(lat.Seconds())
			}
		}(job)
	}
	wg.Wait()
}

// embedTexts computes vectors for the unique texts of one batch. It prefers a
// single batched roundtrip when the provider supports it, falling back to a
// serial loop otherwise. callErr is the representative batch error fed to the
// Governor's Outcome (so an adaptive governor sees a 429's Retry-After); it is
// nil when at least the batch call succeeded.
func (w *Worker) embedTexts(ctx context.Context, uniqueTexts []string) (uniqueVecs [][]float32, failedTexts map[int]bool, callErr error) {
	uniqueVecs = make([][]float32, len(uniqueTexts))
	failedTexts = make(map[int]bool)

	if batchProv, ok := w.embedder.(ports.BatchEmbeddingProvider); ok && len(uniqueTexts) > 1 {
		vecs, err := batchProv.EmbedBatch(ctx, uniqueTexts)
		switch {
		case err == nil:
			copy(uniqueVecs, vecs)
			return uniqueVecs, failedTexts, nil
		case errors.Is(err, ports.ErrBatchEmbedNotSupported):
			// Wrapped provider didn't actually support batch - fall through
			// to the serial path below.
		default:
			// Real batch failure (ErrEmbedderUnreachable, 429, etc.) - mark
			// every unique text as failed; MarkAttemptFailed in persist will
			// bump attempts and (eventually) flip to state='failed'.
			for i := range uniqueTexts {
				failedTexts[i] = true
			}
			return uniqueVecs, failedTexts, err
		}
	}

	var firstErr error
	for i, text := range uniqueTexts {
		if ctx.Err() != nil {
			return uniqueVecs, failedTexts, ctx.Err()
		}
		vec, err := w.embedder.Embed(ctx, text)
		if err != nil {
			failedTexts[i] = true
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		uniqueVecs[i] = vec
	}
	return uniqueVecs, failedTexts, firstErr
}

// persist, enqueueVec, dedupHit, hashEmbedText, and l2Normalize live in
// persist.go - the embed-result -> SQL/vector write stage.
