// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package revalidate

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// PredicateSource exposes the per-node graph predicates Decide needs to re-run
// a rule against current node state. ports.RevalidateQuerier satisfies it (the
// post-promotion Handler passes its repo straight through), and the
// diff-safety gate supplies an ephemeral-graph-backed
// implementation so the SAME decision logic re-runs against a candidate change
// instead of the promoted graph - reuse, not a second copy of the rule
// dispatch.
type PredicateSource interface {
	// HasInboundCallEdges reports whether nodeID currently has >=1 inbound
	// CALLS edge - the dead-code liveness signal. Structural edges (CONTAINS,
	// IMPORTS) are NOT callers and must not count, or a dead symbol reads as
	// live merely because its file/package contains it.
	HasInboundCallEdges(ctx context.Context, repoID, branch, nodeID string) (bool, error)
	// NodeSignaturePair returns the (prev_signature, signature) pair for nodeID.
	NodeSignaturePair(ctx context.Context, repoID, branch, nodeID string) (prev, current string, err error)
	// HasTestCaller reports whether nodeID currently has >=1 direct inbound
	// CALLS caller defined in a test-shaped file - the re-run predicate for the
	// untested-symbol rule.
	HasTestCaller(ctx context.Context, repoID, branch, nodeID string) (bool, error)
}

// Decide re-runs the finding's rule against current node state (read through
// src) and returns the close/refresh decision. It is the single source of
// truth for revalidation rule dispatch, shared by the post-promotion Handler
// and the diff-safety gate's verify path. Reads only - it performs no writes.
// Dispatch:
//
//	"dead-code": close if the anchor now has inbound edges (rule no longer
//	  fires), else refresh in place (still dead).
//	"contract-drift": refresh while prev != "" && prev != current (still
//	  drifting), else close (drift resolved).
//	"untested-symbol": close if the anchor now has a test-file caller (rule no
//	  longer fires - it is covered), else refresh in place (still untested).
//	  Structural twin of dead-code.
//	any other rule: conservative close - rules with no cheap re-run path are
//	  treated as obsolete. Callers that must NOT assume "close == resolved"
//	  for an unsupported rule (e.g. the gate's verify) gate on the rule set
//	  BEFORE calling Decide.
func Decide(ctx context.Context, repoID, branch string, s ports.StaleFinding, src PredicateSource) (ports.FindingDecision, error) {
	switch s.Rule {
	case ruleDeadCode:
		hasIn, err := src.HasInboundCallEdges(ctx, repoID, branch, s.NodeID)
		if err != nil {
			return ports.FindingDecision{}, fmt.Errorf("revalidate.Decide: inbound edges for %q: %w", s.FindingID, err)
		}
		if hasIn {
			// rule no longer fires - the node now has callers.
			return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil
		}
		// still dead - refresh anchor hash in place.
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionRefresh, NewHash: s.CurrentHash}, nil

	case ruleContractDrift:
		prev, cur, err := src.NodeSignaturePair(ctx, repoID, branch, s.NodeID)
		if err != nil {
			return ports.FindingDecision{}, fmt.Errorf("revalidate.Decide: signature pair for %q: %w", s.FindingID, err)
		}
		if prev != "" && prev != cur {
			// still drifting - refresh anchor hash in place.
			return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionRefresh, NewHash: s.CurrentHash}, nil
		}
		// drift resolved (signatures match, or signature absent).
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil

	case ruleUntestedSymbol:
		hasTest, err := src.HasTestCaller(ctx, repoID, branch, s.NodeID)
		if err != nil {
			return ports.FindingDecision{}, fmt.Errorf("revalidate.Decide: test caller for %q: %w", s.FindingID, err)
		}
		if hasTest {
			// rule no longer fires - the symbol now has a test-file caller.
			return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil
		}
		// still untested - refresh anchor hash in place (stays open).
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionRefresh, NewHash: s.CurrentHash}, nil

	default:
		// Unknown rule: conservative close (matches m3.05.2 behavior for
		// rules that have no defined re-run path). Note "auto-link" never
		// reaches here - see the const block in handler.go.
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil
	}
}
