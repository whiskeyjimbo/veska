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
	FilePath   string
	Language   string
	// Text is the deterministic projection used as input for embedding, formatted
	// as "<kind> <symbol_path> <file_path> <language>" with empty trailing fields
	// omitted. file_path and language are included to prevent otherwise-identical
	// symbols in different files or languages from collapsing into a single
	// content-addressed embedding row, while re-promoting the same node in the
	// same file still deduplicates.
	Text string
}

// EmbeddingRefRepo is the worker-side port for node_embedding_refs and the
// content-addressed node_embeddings table.
// The enqueue side (writing pending references) is not part of this port. Refs
// are inserted in the same transaction as the nodes they point at, directly by
// the Promoter. Keeping enqueue out of this port preserves the atomicity contract
// (references and nodes must commit together) and avoids leaking database
// transaction types into the application layer.
type EmbeddingRefRepo interface {
	FetchPending(ctx context.Context, limit int) ([]PendingEmbedRef, error)

	// The result is used to drive the veska_embed_queue_depth gauge.
	CountPending(ctx context.Context) (int, error)

	// MarkReady atomically inserts a row into node_embeddings keyed by contentHash
	// if absent, and updates node_embedding_refs for nodeID to state='ready' with the
	// content hash and timestamp. Both writes must be performed inside a single
	// immediate transaction.
	MarkReady(ctx context.Context, nodeID, contentHash, modelID string, dim int, embedding []byte, at time.Time) error

	// MarkAttemptFailed records one Embed failure for nodeID. The bump-and-maybe-flip
	// is performed in a single UPDATE so concurrent callers cannot observe a torn
	// state. Rows already in state='failed' or 'ready' are not modified.
	MarkAttemptFailed(ctx context.Context, nodeID string, maxAttempts int) error

	// CountByState returns the row count for each of {pending, ready, failed}.
	// Used by the doctor subcommand and the veska_embed_queue_* gauges.
	// States with zero rows are still present in the map with value 0.
	CountByState(ctx context.Context) (map[string]int, error)

	// The hash here is content-addressed on the embed input (modelID + embed_text),
	// so a hit guarantees that an equivalent Embed call has already produced
	// these bytes for this model, allowing reuse.
	LookupExisting(ctx context.Context, contentHash string) (embedding []byte, dim int, found bool, err error)

	// All four negative cases are returned with no error, leaving the caller
	// to decide whether to skip or fail. The (repoID, branch) scoping is enforced
	// via a JOIN to the nodes table because node_embedding_refs is keyed solely by
	// node_id. A bare ref lookup could leak across repositories if node_ids ever collided.
	ContentHashForNode(ctx context.Context, repoID, branch, nodeID string) (contentHash string, ready bool, err error)

	// Reuse marks an existing pending ref as ready without writing to
	// node_embeddings because the row is already present. This fast-path avoids
	// redundantly running the insertion from MarkReady on a known hit.
	Reuse(ctx context.Context, nodeID, contentHash string, at time.Time) error
}
