// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// DecisionKind tags a FindingDecision as either a refresh or a close. Using a
// tagged enum keeps the batch payload in a single contiguous slice, allowing
// the database adapter to iterate over it with two prepared statements in one transaction.
type DecisionKind uint8

const (
	// DecisionRefresh rewrites the anchor content hash on the named open finding,
	// leaving it open.
	DecisionRefresh DecisionKind = iota + 1
	// DecisionClose closes the named open finding as revalidated obsolete.
	DecisionClose
)

// FindingDecision represents a decision on a stale finding. The database adapter
// applies a batch of these decisions under a single transaction to collapse
// what would be multiple fsyncs into a single fsync per file.
type FindingDecision struct {
	FindingID string
	Kind      DecisionKind
	NewHash   string
}

// StaleFinding represents an open finding whose recorded anchor content hash no longer
// matches the current content hash of its anchor node. The struct is narrow so
// callers do not have to round-trip the full domain.Finding aggregate just to
// flip a state column.
type StaleFinding struct {
	// FindingID is the branch-stable identity of the finding.
	FindingID string
	// NodeID is the symbol the finding is anchored on, carried to correlate logs and metrics.
	NodeID string
	// Rule is the rule name that emitted the finding, used to dispatch revalidation handlers.
	Rule string
	// AnchorHash is the content hash captured at emission time.
	AnchorHash string
	// CurrentHash is the present content hash of the anchor node.
	CurrentHash string
}

// RevalidateQuerier is the narrow port used to discover and close stale findings.
// The port is file-scoped because the post-promotion queue enqueues one revalidate
// row per file and work kind. Implementations must be safe for concurrent use.
type RevalidateQuerier interface {
	// StaleFindingsForFile returns all open findings whose recorded anchor content
	// hash differs from the current content hash of the anchor node, scoped to
	// (repoID, branch, filePath). A finding whose anchor node has no row in nodes
	// is not returned because that cleanup is handled by a separate path.
	StaleFindingsForFile(ctx context.Context, repoID, branch, filePath string) ([]StaleFinding, error)

	// HasInboundCallEdges reports if a node has any inbound CALLS edges in the branch.
	// Only CALLS count; structural parent edges (like CONTAINS/IMPORTS) are ignored.
	HasInboundCallEdges(ctx context.Context, repoID, branch, nodeID string) (bool, error)

	// NodeSignaturePair returns the previous and current signature pair. An absent
	// node returns empty strings with no error, which callers treat as a resolved drift.
	NodeSignaturePair(ctx context.Context, repoID, branch, nodeID string) (prev, current string, err error)

	// HasTestCaller reports if a node has at least one direct inbound CALLS caller
	// in a test-shaped file. If a test-file caller exists, the finding is closed;
	// otherwise the row is refreshed in place.
	HasTestCaller(ctx context.Context, repoID, branch, nodeID string) (bool, error)

	// ApplyDecisions applies a batch of refresh and close decisions inside a single
	// transaction to minimize file syncs. If any write fails, all decisions in the
	// batch roll back. Callers must not increment success metrics until ApplyDecisions
	// returns successfully. A matched count of zero during update is not treated as an error.
	ApplyDecisions(ctx context.Context, repoID, branch string, decisions []FindingDecision, at int64) error
}
