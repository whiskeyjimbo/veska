// SPDX-License-Identifier: AGPL-3.0-only

package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"

	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// persist writes one batch's results: each ref looks up the unique slot for
// its content_hash and either persists the new vector (normalizing + INSERT on
// first sight, Reuse for siblings) or marks attempt-failed. Then the batch's
// accumulated vectors are upserted into VectorStorage. Runs serially on the
// drain goroutine - the only place embed results turn into SQL writes.
func (w *Worker) persist(ctx context.Context, job *batchJob) {
	// Build the batch's writes. Skip the embed-result rows when the permit was
	// never acquired: the rows stay pending for a later retry rather than
	// burning an attempt. classify's dedup hits are already in job.readyRefs.
	if job.acquireErr == nil {
		for _, p := range job.needsEmbed {
			slot := job.uniqueByHash[p.contentHash]
			if job.failedTexts[slot] {
				job.failed = append(job.failed, p.ref.NodeID)
				continue
			}
			vec := job.uniqueVecs[slot]
			if len(vec) == 0 {
				continue
			}
			// First time we see this content_hash, normalize + queue the
			// node_embeddings INSERT. Siblings reuse via the inFlight cache;
			// the INSERT's ON CONFLICT covers any cross-job duplicate.
			existing, cached := job.inFlight[p.contentHash]
			if !cached {
				l2Normalize(vec)
				blob := veccodec.EncodeFloat32LE(vec)
				job.inserts = append(job.inserts, ports.EmbedInsert{ContentHash: p.contentHash, Dim: len(vec), Embedding: blob})
				job.inFlight[p.contentHash] = vec
				existing = vec
			} else {
				w.dedupHit()
			}
			job.readyRefs = append(job.readyRefs, ports.EmbedReadyRef{NodeID: p.ref.NodeID, ContentHash: p.contentHash})
			w.enqueueVec(job, p.ref, p.contentHash, existing)
		}
	}

	// One transaction for the whole batch's writes. On failure the rows stay
	// pending; skip the vector upsert so the store never holds vectors for
	// refs the DB still considers unembedded.
	if err := w.refs.ApplyEmbedBatch(ctx, job.inserts, job.readyRefs, job.failed, job.modelID, w.maxAttempts, job.now); err != nil {
		return
	}

	for k, batch := range job.vecBatches {
		// VectorStorage errors are non-fatal; the durable record is the
		// SQL ref row already marked 'ready'. A future retry / reindex
		// path can rebuild the vector store from node_embeddings bytes.
		_ = w.vectors.UpsertEmbeddings(ctx, k.repo, k.branch, batch)
	}
}

func (w *Worker) enqueueVec(job *batchJob, ref ports.PendingEmbedRef, contentHash string, vec []float32) {
	key := vecKey{repo: ref.RepoID, branch: ref.Branch}
	job.vecBatches[key] = append(job.vecBatches[key], domain.EmbeddingRow{
		NodeID:      ref.NodeID,
		ContentHash: contentHash,
		ModelID:     job.modelID,
		Vector:      vec,
	})
}

func (w *Worker) dedupHit() {
	if w.metrics != nil && w.metrics.EmbedDedupHits != nil {
		w.metrics.EmbedDedupHits.Inc()
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
