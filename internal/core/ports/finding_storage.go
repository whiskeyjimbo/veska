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

	// CloseObsolete closes the OPEN finding identified by (findingID, branch),
	// setting closed_reason='revalidated_obsolete'. It is a no-op when no open
	// finding matches — closing an already-closed or absent finding is not an
	// error.
	CloseObsolete(ctx context.Context, findingID, branch string) error

	// CloseSupersededByRule closes every OPEN finding in (repoID, branch, rule)
	// whose finding_id is NOT in keep. An empty keep slice closes every open
	// finding of that rule in the scope.
	//
	// This is the reconciliation primitive for "authoritative" checks: a
	// check that returns the complete set of currently-applicable findings
	// for a given rule on every run (e.g. vulnerable_dependency, which
	// scans the resolved dep set from scratch) needs prior findings to
	// disappear automatically once the underlying condition is resolved.
	// Without it, fixing a vulnerable dep leaves the original finding
	// "open" forever and erodes trust in the findings surface (solov2-jvrc).
	//
	// closed_reason is set to 'revalidated_obsolete' for parity with
	// CloseObsolete / CloseSupersededAutoLinks. The call is idempotent and
	// safe for concurrent use.
	CloseSupersededByRule(ctx context.Context, repoID, branch, rule string, keep []string) error

	// CloseSupersededAutoLinks closes every OPEN finding with rule='auto-link'
	// in (repoID, branch) whose anchor (findings.node_id) is an edge_id of a
	// SIMILAR_TO edge whose src_node_id is in sourceNodeIDs.
	//
	// This is the supersession step the auto-link handler runs before writing
	// a fresh batch of candidates for a given source-file: prior auto-link
	// findings whose target choice has since drifted (a different nearest
	// neighbour, dropped below threshold, …) are explicitly closed so the
	// "open findings" surface does not balloon across re-promotions
	// (solov2-ok7y).
	//
	// closed_reason is set to 'revalidated_obsolete' for parity with
	// CloseObsolete. An empty sourceNodeIDs slice is a no-op. The call is
	// idempotent: already-closed findings are left untouched.
	CloseSupersededAutoLinks(ctx context.Context, repoID, branch string, sourceNodeIDs []string) error
}
