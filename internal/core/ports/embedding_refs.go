package ports

import (
	"context"
	"time"
)

// PendingEmbedRef is a single pending row from node_embedding_refs joined with
// the minimal node fields the embedder worker needs to construct an embedding
// input string and a vector-store row.
type PendingEmbedRef struct {
	NodeID     string
	RepoID     string
	Branch     string
	SymbolPath string
	Kind       string
	// Text is the deterministic projection used as Embed input.
	// m3.02.1 uses "<kind> <symbol_path>"; this is documented in the commit
	// message and may be refined later without changing the schema.
	Text string
}

// EmbeddingRefRepo is the worker-side port for node_embedding_refs and the
// content-addressed node_embeddings table.
//
// The enqueue side (writing pending refs) is NOT part of this port: refs are
// inserted in the same transaction as the nodes they point at, directly by
// Promoter. Keeping enqueue out of this port preserves the atomicity contract
// (refs + nodes must commit together) and avoids leaking *sql.Tx into the
// application layer.
type EmbeddingRefRepo interface {
	// FetchPending returns up to limit pending refs (state='pending'),
	// oldest-first by enqueued_at. The returned slice may be shorter than
	// limit; an empty slice means the queue is drained.
	FetchPending(ctx context.Context, limit int) ([]PendingEmbedRef, error)

	// CountPending returns the number of rows currently in state='pending'.
	// Used to drive the veska_embed_queue_depth gauge.
	CountPending(ctx context.Context) (int, error)

	// MarkReady atomically:
	//  - inserts a row into node_embeddings keyed by contentHash if absent
	//    (ON CONFLICT DO NOTHING — idempotent w.r.t. content),
	//  - updates node_embedding_refs for nodeID to state='ready',
	//    content_hash=contentHash, embedded_at=at.
	//
	// Both writes are performed inside a single BEGIN IMMEDIATE transaction.
	MarkReady(ctx context.Context, nodeID, contentHash, modelID string, dim int, embedding []byte, at time.Time) error
}
