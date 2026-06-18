package application

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)


var workKinds = []string{
	string(ports.WorkKindEmbed),
	string(ports.WorkKindAutoLink),
	string(ports.WorkKindRevalidate),
}

// PromotionWorkKinds returns the enqueued work types. The 'review' lane is
// optional and only enqueued if reviewEnabled is true.
func PromotionWorkKinds(reviewEnabled bool) []string {
	out := make([]string, len(workKinds), len(workKinds)+1)
	copy(out, workKinds)
	if reviewEnabled {
		out = append(out, string(ports.WorkKindReview))
	}
	return out
}

// ErrUnregisteredRepo is returned by PromotionStore.Promote when repoID is not
// registered in the repository table.
type ErrUnregisteredRepo struct{ RepoID string }

func (e ErrUnregisteredRepo) Error() string {
	return fmt.Sprintf(
		"promoter: repo %q is not registered - run: veska repo add <path>",
		e.RepoID,
	)
}

// PromotionFile contains the parsed nodes and edges for a staged file.
// Vector-similarity edges (SIMILAR_TO) are computed later by the autolink worker.
type PromotionFile struct {
	Path  string
	Nodes []*domain.Node
	Edges []*domain.Edge
	// UnresolvedCalls lists calls to packages resolved during promotion.
	UnresolvedCalls []domain.UnresolvedCall
	// Imports maps local packages to import paths for cross-reference.
	Imports map[string]string
}

// PromotionBatch holds the metadata and staged files for an atomic promotion,
// using a consistent timestamp for all rows.
type PromotionBatch struct {
	RepoID     string
	Branch     string
	GitSHA     string
	Actor      domain.Actor
	PromotedAt int64
	Files      []PromotionFile
}

// PromotionStore abstracts the transaction layer to write nodes, FTS indexes,
// embedding references, and queues atomically.
type PromotionStore interface {
	Promote(ctx context.Context, batch PromotionBatch) error
}
