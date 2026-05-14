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

	// MarkAttemptFailed records one Embed failure for nodeID:
	//   - increments node_embedding_refs.attempts by 1,
	//   - flips state to 'failed' if the new attempts value is >= maxAttempts.
	//
	// The bump-and-maybe-flip is performed in a single UPDATE so concurrent
	// callers cannot observe a torn state. Rows already in state='failed'
	// or 'ready' are not modified (the WHERE clause restricts to pending).
	MarkAttemptFailed(ctx context.Context, nodeID string, maxAttempts int) error

	// CountByState returns the row count for each of {pending, ready, failed}.
	// Used by the doctor subcommand and the veska_embed_queue_* gauges.
	// States with zero rows are still present in the map with value 0.
	CountByState(ctx context.Context) (map[string]int, error)

	// LookupExisting returns the stored embedding bytes and dimension for
	// contentHash if a row exists in node_embeddings. found=false with no
	// error means "miss" (the caller must Embed and MarkReady). The hash
	// here is content-addressed on the EMBED INPUT (modelID + embed_text),
	// so a hit is a guarantee that an equivalent Embed call has already
	// produced these bytes for this model — the worker can reuse them.
	LookupExisting(ctx context.Context, contentHash string) (embedding []byte, dim int, found bool, err error)

	// ContentHashForNode returns the content_hash of the embedding for nodeID
	// scoped to (repoID, branch), with a ready flag.
	//
	//   ready=true with a non-empty hash: the ref is in state='ready' and the
	//     hash points at a row in node_embeddings.
	//   ready=false: the node has no ref row, the ref is state='pending'
	//     (no hash yet), state='failed', or the node does not match
	//     (repoID, branch). All four cases are returned with err=nil — the
	//     caller decides whether to skip or fail.
	//
	// The (repoID, branch) scoping is enforced via a JOIN to the nodes table:
	// node_embedding_refs is keyed solely by node_id (the nodes table owns the
	// repo/branch dimension), so a bare ref lookup could leak across repos if
	// node_ids ever collided. The JOIN closes that hole.
	ContentHashForNode(ctx context.Context, repoID, branch, nodeID string) (contentHash string, ready bool, err error)

	// Reuse marks an existing pending ref as ready against contentHash
	// WITHOUT writing node_embeddings (the row is already there — see
	// LookupExisting). Used by the dedup fast-path so we don't redundantly
	// run the INSERT…ON CONFLICT DO NOTHING from MarkReady on a known hit.
	Reuse(ctx context.Context, nodeID, contentHash string, at time.Time) error
}
