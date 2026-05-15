package ports

import "context"

// StaleFinding is one open finding whose recorded anchor content hash no longer
// matches the current content_hash of its anchor node. The revalidation sweep
// uses this view to drive the close-as-obsolete path.
//
// All four fields are scoped by (repo_id, branch) carried at the call site;
// the struct is intentionally narrow so callers do not have to round-trip the
// full domain.Finding aggregate just to flip a state column.
type StaleFinding struct {
	// FindingID is the branch-stable identity that, together with the call
	// site's branch, uniquely names the row in the findings table.
	FindingID string
	// NodeID is the symbol the finding is anchored on (findings.node_id).
	// Carried so callers can correlate logs / metrics without re-querying.
	NodeID string
	// AnchorHash is the content_hash captured on the finding at emission
	// time (findings.anchor_content_hash). Always non-empty in results
	// returned by StaleFindingsForFile (the query filters NULLs).
	AnchorHash string
	// CurrentHash is the present content_hash of the anchor node
	// (nodes.content_hash). Guaranteed to differ from AnchorHash for any
	// row returned by StaleFindingsForFile.
	CurrentHash string
}

// RevalidateQuerier is the narrow port the revalidation sweep handler uses to
// (1) discover findings whose anchor has drifted on a given file and
// (2) close those findings as revalidated-obsolete.
//
// The port is intentionally file-scoped: the post-promotion queue enqueues one
// revalidate row per file × work_kind, and the handler operates on exactly
// that file's blast radius. Cross-file or cross-branch sweeps are out of scope
// for this port; the queue produces those as separate rows.
//
// Implementations must be safe for concurrent use.
type RevalidateQuerier interface {
	// StaleFindingsForFile returns all open findings whose recorded
	// anchor_content_hash differs from the current nodes.content_hash of
	// the anchor node, scoped to (repoID, branch, filePath).
	//
	// Filters applied:
	//   - findings.state = 'open'
	//   - findings.anchor_content_hash IS NOT NULL
	//   - nodes.file_path = filePath
	//   - nodes.content_hash != findings.anchor_content_hash
	//
	// A finding whose anchor node has no row in `nodes` (e.g. node was
	// deleted on the latest promotion) is NOT returned by this query —
	// that's a separate cleanup path that 5.2 deliberately does not own.
	StaleFindingsForFile(ctx context.Context, repoID, branch, filePath string) ([]StaleFinding, error)

	// CloseAsRevalidatedObsolete flips the named finding to
	// state='closed', closed_reason='revalidated_obsolete', stamping
	// closed_at and the system actor. Idempotent: re-closing an
	// already-closed finding is a no-op (UPDATE matches nothing or
	// matches an already-closed row; both paths return nil).
	CloseAsRevalidatedObsolete(ctx context.Context, repoID, branch, findingID string, closedAt int64) error
}
