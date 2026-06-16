package ports

import "context"

// DecisionKind tags a FindingDecision as either a refresh-in-place or a
// close-as-revalidated-obsolete. Using a tagged enum (rather than separate
// Refresh / Close concrete types) keeps the batch payload a single contiguous
// slice that the SQLite adapter can iterate over with two prepared statements
// inside one transaction.
type DecisionKind uint8

const (
	// DecisionRefresh rewrites findings.anchor_content_hash to NewHash on the
	// named open finding. State stays 'open'; closed_reason stays NULL.
	DecisionRefresh DecisionKind = iota + 1
	// DecisionClose flips the named open finding to state='closed' with
	// closed_reason='revalidated_obsolete'. NewHash is ignored.
	DecisionClose
)

// FindingDecision is one entry in a batch passed to
// RevalidateQuerier.ApplyDecisions. The handler builds a slice of these (one
// per stale finding on a given file) and the SQLite adapter applies all of
// them under a single transaction — collapsing what was previously O(stale)
// fsyncs into a single fsync per file.
//
// Field semantics:
//   - FindingID is the row key (combined with (repoID, branch) at the call site).
//   - Kind selects the SQL path (refresh vs close).
//   - NewHash carries the new anchor_content_hash for DecisionRefresh; it is
//     ignored for DecisionClose and may be empty.
type FindingDecision struct {
	FindingID string
	Kind      DecisionKind
	NewHash   string
}

// StaleFinding is one open finding whose recorded anchor content hash no longer
// matches the current content_hash of its anchor node. The revalidation sweep
// uses this view to drive the per-rule dispatch (refresh vs close).
//
// All five fields are scoped by (repo_id, branch) carried at the call site;
// the struct is intentionally narrow so callers do not have to round-trip the
// full domain.Finding aggregate just to flip a state column.
type StaleFinding struct {
	// FindingID is the branch-stable identity that, together with the call
	// site's branch, uniquely names the row in the findings table.
	FindingID string
	// NodeID is the symbol the finding is anchored on (findings.node_id).
	// Carried so callers can correlate logs / metrics without re-querying.
	NodeID string
	// Rule is the rule name that emitted this finding (findings.rule).
	// The revalidate handler dispatches on Rule to decide whether the rule
	// can be cheaply re-run on current node state (dead-code, contract-drift)
	// or whether the finding is always closed (auto-link, unknown).
	Rule string
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

	// HasInboundCallEdges reports whether the named node currently has at least
	// one inbound CALLS edge on (repoID, branch). Used by the revalidation
	// handler's dead-code re-run: if a stale dead-code finding's anchor node
	// now has a caller, the rule no longer fires and the finding is closed as
	// obsolete. If it still has zero inbound CALLS edges, the rule still fires
	// and the finding row is REFRESHED in place. Only CALLS count — a
	// structural CONTAINS/IMPORTS parent edge is not a caller, so it must not
	// resolve a dead-code finding (solov2-nmps.9).
	HasInboundCallEdges(ctx context.Context, repoID, branch, nodeID string) (bool, error)

	// NodeSignaturePair returns the (prev_signature, signature) pair for
	// the named node on (repoID, branch). Used by the revalidation
	// handler's contract-drift re-run: if prev != "" && prev != current,
	// the contract still drifts; otherwise the drift is resolved and the
	// finding is closed as obsolete. Returns ("", "", nil) when the node
	// is absent — caller treats that as "drift resolved" (no longer fires).
	NodeSignaturePair(ctx context.Context, repoID, branch, nodeID string) (prev, current string, err error)

	// HasTestCaller reports whether the named node currently has at least one
	// direct inbound CALLS caller defined in a test-shaped file on (repoID,
	// branch). Used by the revalidation handler's untested-symbol re-run: if a
	// stale untested-symbol finding's anchor now has a test-file caller, the
	// rule no longer fires (it is covered) and the finding is closed as
	// obsolete; otherwise it is still untested and the row is REFRESHED in place.
	HasTestCaller(ctx context.Context, repoID, branch, nodeID string) (bool, error)

	// ApplyDecisions applies a batch of refresh and/or close decisions to
	// open findings on (repoID, branch) in a single transaction. The intent
	// is to collapse the per-file revalidation sweep — which can produce
	// thousands of UPDATEs on large commits — into one fsync per file
	// instead of one fsync per finding.
	//
	// Semantics:
	//   - Empty decisions slice: no-op, no transaction opened, returns nil.
	//   - Each DecisionRefresh updates findings.anchor_content_hash to
	//     d.NewHash on (finding_id, branch, repo_id) gated on state='open'.
	//   - Each DecisionClose flips state='closed' with closed_reason=
	//     'revalidated_obsolete', closed_at=at, actor_id='service:veska',
	//     actor_kind='system', gated on state='open'.
	//   - Per-row UPDATE-matched-zero is NOT an error (a row that was
	//     already closed by another path is the normal case).
	//   - If any step of the tx fails (incl. Commit), all decisions in the
	//     batch roll back and an error is returned wrapping the underlying
	//     driver error. Callers must NOT increment success metrics until
	//     ApplyDecisions returns nil.
	//
	// The `at` parameter stamps closed_at on close decisions. Refresh
	// decisions ignore it for now (no last_revalidated_at column), mirroring
	// RefreshAnchorHash.
	ApplyDecisions(ctx context.Context, repoID, branch string, decisions []FindingDecision, at int64) error
}
