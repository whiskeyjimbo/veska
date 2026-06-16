package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// EdgeStorage is the port for persisting domain.Edge values produced by the
// auto-link pipeline and any future analyser that emits structural or
// proposed edges as a batch.
// SaveEdges must be safe for concurrent use and idempotent on the
// (edge_id, branch) primary key: re-saving the same edge must not error,
// duplicate the row, or downgrade an already-resolved edge to Unresolved.
// The first writer wins for identity and confidence; an implementation MAY
// refresh a mutable strength signal (e.g. domain.Edge.Score) on conflict
// without violating this contract.
// An empty edges slice is a no-op (nil error, no round-trip).
type EdgeStorage interface {
	SaveEdges(ctx context.Context, repoID, branch string, edges []*domain.Edge) error
}
