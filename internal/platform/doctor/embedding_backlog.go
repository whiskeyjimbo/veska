// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import "context"

// EmbeddingBacklogReport summarizes the depth of the embedder backfill queue
// for `veska doctor status`. Unlike EmbedQueueReport (which classifies the
// embedder *worker* and uses "healthy"/"degraded"/"broken"), this report is
// purely informational: it surfaces the queue depth so the doctor rollup
// agrees with the agent-facing `eng_get_status` signal.
// Status values:
//
//	"drained" - Pending == 0; the embedder has nothing left to do.
//	"backfilling" - Pending > 0; the embedder is working through a backlog.
//	  This is NOT classified as "degraded" in the rollup - the daemon
//	  (embedder worker, queue, ingestion) is healthy, work just isn't done
//	  yet. The same backlog DOES drive `eng_get_status`'s
//	  `degraded_reasons:["embeddings_pending"]` because that signal tells
//	  agents to choose between semantic and lexical search paths; doctor's
//	  audience is operators reading a go/no-go, who care that the daemon
//	  is functioning, not that semantic search is mid-warmup.
//	"unknown" - counter is nil or the underlying query failed. Surfaces
//	  "we couldn't measure" without poisoning the rollup as a real fault.
type EmbeddingBacklogReport struct {
	Pending int    `json:"pending"`
	Status  string `json:"status"`
}

// pendingCounter is the minimal surface CheckEmbeddingBacklog needs. Defined
// here (rather than imported from sqlite) so the doctor package stays
// unidirectional. Production callers pass *sqlite.EmbeddingRefsRepo, which
// satisfies this shape via its CountPending method.
type pendingCounter interface {
	CountPending(ctx context.Context) (int, error)
}

// CheckEmbeddingBacklog returns a presentation-only view of the embedder
// backfill queue depth. See EmbeddingBacklogReport for status semantics.
// This probe is deliberately decoupled from CheckEmbedQueueHealth: that
// probe judges the embedder worker's correctness (failed rows, very deep
// backlog) and DOES drive the rollup; this one only mirrors what
// `eng_get_status` reports as `pending_embeds`, so doctor and the MCP
// status surface point at the same number even though they classify it
// differently for different audiences.
func CheckEmbeddingBacklog(ctx context.Context, refs pendingCounter) (EmbeddingBacklogReport, error) {
	if refs == nil {
		return EmbeddingBacklogReport{Status: "unknown"}, nil
	}
	pending, err := refs.CountPending(ctx)
	if err != nil {
		return EmbeddingBacklogReport{Status: "unknown"}, nil
	}
	status := "drained"
	if pending > 0 {
		status = "backfilling"
	}
	return EmbeddingBacklogReport{Pending: pending, Status: status}, nil
}
