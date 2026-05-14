package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// FindingStorage is the port for persisting Findings produced by structural,
// semantic, security, or quality checks. Implementations are provided by
// infrastructure adapters (e.g. the SQLite findings table).
//
// Save must be safe for concurrent use. Save is expected to be idempotent on
// the (finding_id, branch) primary key: re-saving the same finding must not
// error or create duplicate rows.
type FindingStorage interface {
	// Save persists f. The caller retains ownership of f and Save must not
	// mutate it.
	Save(ctx context.Context, f *domain.Finding) error
}
