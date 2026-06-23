// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"
	"errors"
)

// ErrDeferWork is returned by a WorkHandler when a row cannot be processed yet
// but is NOT a failure: a transient precondition (e.g. the file's embeddings
// are still pending) is unmet. The poller re-queues the row with a short
// availability delay WITHOUT consuming its retry budget, so the row is retried
// later instead of busy-looping or failing permanently.
var ErrDeferWork = errors.New("queue: work deferred, precondition not yet met")

// WorkKind identifies the type of post-promotion work. The values mirror the
// database column in post_promotion_queue.
type WorkKind string

const (
	WorkKindEmbed      WorkKind = "embed"
	WorkKindAutoLink   WorkKind = "auto_link"
	WorkKindRevalidate WorkKind = "revalidate"
	WorkKindReview     WorkKind = "review"
	WorkKindWiki       WorkKind = "wiki"
	WorkKindSummary    WorkKind = "summary"
	// WorkKindFTS rebuilds a file's full-text-search rows asynchronously after
	// promotion. The expensive FTS5 inserts used to run co-transactionally in
	// the promote tx (~42s of an 844-file cold scan); deferring them keeps the
	// promote critical path short. Orphan cleanup for deleted/renamed symbols
	// still happens synchronously in ftsSink.BeforeNodeDelete, while the old
	// node rows are still present to identify them.
	WorkKindFTS WorkKind = "fts"
)

type WorkRow struct {
	Seq         int64
	PromotionID string
	RepoID      string
	Branch      string
	GitSHA      string
	Kind        WorkKind
	Payload     string
	State       string
	Attempts    int
	EnqueuedAt  int64
}

// WorkHandler processes one queue row. A returned error tells the poller to
// either re-queue (when attempts < 3) or mark the row failed, while a nil error
// indicates success. Implementations must be safe for concurrent use.
type WorkHandler interface {
	Handle(ctx context.Context, row WorkRow) error
}
