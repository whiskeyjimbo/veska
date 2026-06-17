package ports

import "context"

// WorkKind identifies the type of post-promotion work. The values mirror the
// database column in post_promotion_queue.
type WorkKind string

const (
	WorkKindEmbed      WorkKind = "embed"
	WorkKindAutoLink   WorkKind = "auto_link"
	WorkKindRevalidate WorkKind = "revalidate"
	WorkKindReview     WorkKind = "review"
	WorkKindWiki       WorkKind = "wiki"
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
