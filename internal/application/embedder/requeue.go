package embedder

import (
	"context"
	"database/sql"
	"fmt"
)

// RequeueAllUnderNewModel wipes the content-addressed embedding store and
// resets every embedding-ref to pending, so the embedder worker re-embeds all
// promoted nodes under the currently-elected model.
//
// Why needed (solov2-fz8): node_embeddings is keyed by content_hash with
// ON CONFLICT(content_hash) DO NOTHING — re-embedding the same content under
// a different model would otherwise be a no-op and keep the old-model vector.
// And the sqlite-vec store is in-memory, rehydrated from node_embeddings at
// boot — so this MUST run before vector rehydration to start the store empty.
//
// Returns the number of refs flipped back to pending so the daemon log can
// surface "auto-reindex N nodes" instead of the previous warn-only behaviour.
func RequeueAllUnderNewModel(ctx context.Context, writeDB *sql.DB) (int64, error) {
	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("requeue embeddings: begin: %w", err)
	}
	// Reset refs FIRST: node_embedding_refs.content_hash has a FK into
	// node_embeddings, so the embeddings can only be cleared after every ref
	// has dropped its reference.
	res, err := tx.ExecContext(ctx, `UPDATE node_embedding_refs
		SET state = 'pending', content_hash = NULL, embedded_at = NULL, attempts = 0`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("requeue embeddings: reset refs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_embeddings`); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("requeue embeddings: clear node_embeddings: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("requeue embeddings: commit: %w", err)
	}
	return n, nil
}
