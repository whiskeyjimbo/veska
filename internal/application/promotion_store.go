package application

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// workKinds lists the always-on post-promotion work kinds enqueued per file. A
// PromotionStore enqueues one post_promotion_queue row per file per work kind.
var workKinds = []string{
	string(ports.WorkKindEmbed),
	string(ports.WorkKindAutoLink),
	string(ports.WorkKindRevalidate),
}

// PromotionWorkKinds returns the list of post-promotion work kinds a
// PromotionStore enqueues — one queue row per file per kind. It is exported so
// the infrastructure adapter can drive the queue inserts without re-declaring
// the canonical list.
//
// reviewEnabled gates the optional WorkKindReview lane: when true, 'review' is
// appended so a review row is enqueued per changed file; when false (the
// default), no review row is enqueued. The always-on kinds are unconditional.
func PromotionWorkKinds(reviewEnabled bool) []string {
	out := make([]string, len(workKinds), len(workKinds)+1)
	copy(out, workKinds)
	if reviewEnabled {
		out = append(out, string(ports.WorkKindReview))
	}
	return out
}

// ErrUnregisteredRepo is returned by PromotionStore.Promote when the repoID is
// not found in the repos table. The daemon must never promote work from an
// unknown repo. It is type-assertable via errors.As by callers.
type ErrUnregisteredRepo struct{ RepoID string }

func (e ErrUnregisteredRepo) Error() string {
	return fmt.Sprintf(
		"promoter: repo %q is not registered — run: veska repo add <path>",
		e.RepoID,
	)
}

// PromotionFile is the set of nodes (and parser-produced edges) parsed for a
// single staged file. Edges carry structural relationships the parser can
// determine at parse time (CALLS, IMPORTS, etc., per solov2-ijg).
// Vector-similarity edges (SIMILAR_TO) are NOT in this set — those are
// derived post-promotion by the autolink queue worker.
type PromotionFile struct {
	Path  string
	Nodes []*domain.Node
	Edges []*domain.Edge
}

// PromotionBatch is the plain-data description of one promotion. It carries no
// SQL types so it can cross the application/infrastructure boundary. The
// PromotionStore is responsible for turning it into an atomic transaction.
//
// PromotedAt is computed by the Promoter (now-millis) so a single timestamp is
// applied consistently to every row in the batch and stays deterministic for
// tests.
type PromotionBatch struct {
	RepoID     string
	Branch     string
	GitSHA     string
	Actor      domain.Actor
	PromotedAt int64
	Files      []PromotionFile
}

// PromotionStore is the port through which the Promoter flushes a batch of
// staged nodes to durable storage. The implementation owns the ENTIRE
// transaction — registration check through commit — so that all node, FTS,
// embedding-ref and queue writes land atomically or not at all.
//
// Promote returns ErrUnregisteredRepo when the batch's repo is not registered.
type PromotionStore interface {
	Promote(ctx context.Context, batch PromotionBatch) error
}
