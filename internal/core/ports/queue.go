package ports

import "context"

// WorkKind identifies the type of post-promotion work the queue is
// dispatching. The values mirror the work_kind TEXT column in the
// post_promotion_queue table; new kinds are added here first and the
// infrastructure poller picks them up structurally.
type WorkKind string

const (
	WorkKindEmbed      WorkKind = "embed"
	WorkKindAutoLink   WorkKind = "auto_link"
	WorkKindRevalidate WorkKind = "revalidate"
	WorkKindReview     WorkKind = "review"
)

// WorkRow is the application-facing view of one row pulled from the
// post-promotion queue. The infrastructure adapter (sqlite/queue) hydrates
// these from the underlying SQL row before dispatching to a WorkHandler.
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

// WorkHandler processes one queue row. Implementations live in the
// application layer (one per WorkKind) and are wired into the infrastructure
// poller at start-up.
//
// A returned error tells the poller to either re-queue (attempts < 3) or
// mark the row failed; nil indicates success. Implementations must be safe
// for concurrent use even though the poller runs one goroutine per kind.
type WorkHandler interface {
	Handle(ctx context.Context, row WorkRow) error
}
