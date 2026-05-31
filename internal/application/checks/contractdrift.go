package checks

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ContractDriftCheck is a structural check that flags symbols whose
// declaration signature changed between two consecutive promotions on the
// same branch. The comparison is anchored on (node_id, branch) and uses the
// per-row prev_signature/signature columns written by the Promoter, so the
// check does not need to maintain its own history table.
//
// Scope:
//
//   - Only nodes of kind function, method, or interface are considered.
//   - Both prev_signature and signature must be non-empty — first-time
//     promotions (no prior row) cannot drift by construction.
//   - Body-only edits leave signature unchanged and so emit no finding.
//
// Findings are anchored on node_id so finding_id is branch-stable and
// idempotent re-runs collapse via the storage layer's
// ON CONFLICT(finding_id, branch) DO UPDATE clause.
type ContractDriftCheck struct {
	q ports.ContractDriftQuerier
}

// NewContractDriftCheck constructs a ContractDriftCheck bound to q. The
// querier is required; passing nil causes Run to return an error on first
// invocation.
func NewContractDriftCheck(q ports.ContractDriftQuerier) *ContractDriftCheck {
	return &ContractDriftCheck{q: q}
}

// Name returns the Prometheus / finding-rule attribution name.
func (c *ContractDriftCheck) Name() string { return "contract-drift" }

// Run loads the set of drifted nodes from the querier for the input's file
// paths and constructs one Finding per node. Findings carry the before/after
// signature snippet in their message so reviewers can see what changed
// without a second round-trip.
//
// An empty Input.FilePaths is a no-op.
func (c *ContractDriftCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.q == nil {
		return nil, fmt.Errorf("contract-drift: nil querier")
	}
	if len(in.FilePaths) == 0 {
		return nil, nil
	}

	drifted, err := c.q.DriftedNodesInFiles(ctx, in.RepoID, in.Branch, in.FilePaths)
	if err != nil {
		return nil, fmt.Errorf("contract-drift: query: %w", err)
	}

	out := make([]*domain.Finding, 0, len(drifted))
	for _, d := range drifted {
		msg := fmt.Sprintf(
			"signature of %s %q in %s changed on branch %s: %q -> %q",
			d.Kind, d.Name, d.FilePath, in.Branch, d.PrevSig, d.NewSig,
		)
		// Capture the drifted node's CURRENT content_hash so the revalidation
		// sweep (m3.05.2) recognises a subsequent drift and supersedes this
		// finding. An empty hash from the adapter falls back to NULL.
		opts := []domain.FindingOption{domain.WithNodeAnchor(d.NodeID)}
		if d.ContentHash != "" {
			opts = append(opts, domain.WithAnchorContentHash(d.ContentHash))
		}
		f, err := domain.NewFinding(domain.FindingSpec{
			RepoID:   in.RepoID,
			Branch:   in.Branch,
			Severity: domain.SeverityMedium,
			Layer:    domain.LayerStructural,
			Rule:     "contract-drift",
			Message:  msg,
		}, opts...)
		if err != nil {
			// A malformed node ref should not abort the whole check; skip it.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}
