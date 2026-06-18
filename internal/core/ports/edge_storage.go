// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// EdgeStorage is the port for persisting domain.Edge values.
// SaveEdges must be safe for concurrent use and idempotent on the
// (edge_id, branch) primary key. Re-saving the same edge must not error,
// duplicate the row, or downgrade an already-resolved edge to Unresolved.
// The first writer wins for identity and confidence; implementations may
// refresh a mutable strength signal (e.g., domain.Edge.Score) on conflict.
// An empty edges slice is a no-op.
type EdgeStorage interface {
	SaveEdges(ctx context.Context, repoID, branch string, edges []*domain.Edge) error
}
