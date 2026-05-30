// Package revalidate implements the post-promotion sweep that revalidates
// open findings whose anchor symbol has drifted on disk.
//
// One queue row -> one file. The handler asks the RevalidateQuerier port for
// the set of open findings on that file whose recorded anchor_content_hash
// no longer matches the current node's content_hash, and then dispatches
// per-rule:
//
//   - "dead-code": cheap re-run. If the anchor node now has >=1 inbound
//     edge, the rule no longer fires and the finding is closed as
//     'revalidated_obsolete'. If it still has zero inbound edges, the rule
//     still fires on the new content — the existing row is REFRESHED in
//     place (anchor_content_hash := current_hash; state stays 'open').
//
//   - "contract-drift": cheap re-run. Read the node's (prev_signature,
//     signature) pair. If prev != "" && prev != current, drift still
//     applies → REFRESH. Otherwise → CLOSE as 'revalidated_obsolete'.
//
//   - "auto-link": NOT swept. Auto-link findings store an edge_id in the
//     findings.node_id column; StaleFindingsForFile joins findings.node_id
//     to nodes.node_id, which an edge_id never matches — so auto-link
//     findings are never selected here. Their lifecycle is developer-driven
//     (accept/suppress); a stale similarity suggestion is low-harm. An
//     edge-anchored revalidation path would be a separate feature.
//
//   - any other rule: conservative default — close. New rules opt into
//     refresh behaviour by adding a case here.
//
// Why no "superseded_by_revalidation" closed_reason: finding IDs are
// branch-stable hash(rule + anchor). For a node-anchored finding, re-firing
// the rule produces the SAME finding_id, so "the rule still fires on new
// content" never produces a new row — it just means the existing row is
// still valid. Refresh the anchor hash and move on.
//
// Scope discipline:
//   - File-bounded: scope = (payload file path).
//   - Branch-bounded: scope = row.Branch.
//   - Repo-bounded:   scope = row.RepoID.
//   - NULL anchor_content_hash: untouched (file-anchored findings have no
//     hash to compare against).
//   - Already-closed findings: untouched (UPDATE gated on state='open').
package revalidate

import (
	"context"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// Rule names recognised by the per-rule dispatch. They are duplicated here
// (not imported from internal/application/checks or autolink) to keep the
// application/revalidate package free of inbound deps on its sibling
// packages — the rule name on a stored Finding is the wire contract.
//
// "auto-link" is intentionally NOT listed: auto-link findings store an
// edge_id in the findings.node_id column, but StaleFindingsForFile selects
// stale findings via JOIN nodes ON nodes.node_id = findings.node_id — an
// edge_id never matches a nodes row, so auto-link findings are never picked
// up by this sweep. Their lifecycle is developer-driven (accept/suppress);
// see solov2-l8y. An edge-anchored revalidation path would be a new feature,
// not a per-rule case here.
const (
	ruleDeadCode      = "dead-code"
	ruleContractDrift = "contract-drift"
)

// Handler implements ports.WorkHandler for WorkKindRevalidate rows.
//
// Failure semantics mirror autolink.Handler: SQL errors propagate wrapped so
// queue.Poller's retry path runs; programmer errors (wrong WorkKind) return
// a wrapped error so misrouting is loud.
type Handler struct {
	repo    ports.RevalidateQuerier
	clock   func() time.Time
	metrics *observability.Metrics
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithClock replaces the wall-clock used for the closed_at / refresh-at
// stamp. Primarily for tests. The default is time.Now.
func WithClock(c func() time.Time) Option {
	return func(h *Handler) {
		if c != nil {
			h.clock = c
		}
	}
}

// WithMetrics attaches a Metrics struct so the handler can increment
// veska_revalidate_closed_total and veska_revalidate_refreshed_total. A
// nil-metrics handler is fully functional and simply does not emit either
// counter.
func WithMetrics(m *observability.Metrics) Option {
	return func(h *Handler) {
		h.metrics = m
	}
}

// NewHandler constructs a Handler bound to the given RevalidateQuerier.
// repo is required; nil panics at construction time to mirror autolink.
func NewHandler(repo ports.RevalidateQuerier, opts ...Option) *Handler {
	if repo == nil {
		panic("revalidate.NewHandler: repo is nil")
	}
	h := &Handler{
		repo:  repo,
		clock: time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Handle processes a single ports.WorkRow of kind WorkKindRevalidate.
//
// Behaviour:
//   - Wrong kind: wrapped error (routing bug).
//   - Empty payload: nil (no file => nothing to sweep).
//   - SQL error from any port method: wrapped error so the Poller retries.
//   - Per stale finding: dispatch by Rule (see package doc).
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindRevalidate {
		return fmt.Errorf("revalidate.Handle: unexpected kind %q", row.Kind)
	}
	filePath := row.Payload
	if filePath == "" {
		return nil
	}

	stale, err := h.repo.StaleFindingsForFile(ctx, row.RepoID, row.Branch, filePath)
	if err != nil {
		return fmt.Errorf("revalidate.Handle: stale findings for %q: %w", filePath, err)
	}
	if len(stale) == 0 {
		return nil
	}

	at := h.clock().UnixMilli()

	// Phase 1: compute per-rule decisions WITHOUT writing. Reads
	// (HasInboundEdges, NodeSignaturePair) stay outside the write tx so the
	// transaction stays as short as possible and only contains UPDATEs.
	decisions := make([]ports.FindingDecision, 0, len(stale))
	var refreshed, closed int
	for _, s := range stale {
		d, err := h.decide(ctx, row, s)
		if err != nil {
			return err
		}
		decisions = append(decisions, d)
		if d.Kind == ports.DecisionRefresh {
			refreshed++
		} else {
			closed++
		}
	}

	// Phase 2: one transaction, one fsync per file. If commit fails, no
	// metrics are bumped — queue.Poller will retry the row and the same
	// decisions will be re-derived on the next attempt.
	if err := h.repo.ApplyDecisions(ctx, row.RepoID, row.Branch, decisions, at); err != nil {
		return fmt.Errorf("revalidate.Handle: apply decisions on %q: %w", filePath, err)
	}

	// Phase 3: bump metrics by the counts of each kind in the batch.
	if h.metrics != nil {
		if h.metrics.RevalidateRefreshed != nil && refreshed > 0 {
			h.metrics.RevalidateRefreshed.Add(float64(refreshed))
		}
		if h.metrics.RevalidateClosed != nil && closed > 0 {
			h.metrics.RevalidateClosed.Add(float64(closed))
		}
	}
	return nil
}

// decide derives a FindingDecision for one stale finding using per-rule
// re-run logic. Reads only — no writes happen until ApplyDecisions.
func (h *Handler) decide(ctx context.Context, row ports.WorkRow, s ports.StaleFinding) (ports.FindingDecision, error) {
	switch s.Rule {
	case ruleDeadCode:
		hasIn, err := h.repo.HasInboundEdges(ctx, row.RepoID, row.Branch, s.NodeID)
		if err != nil {
			return ports.FindingDecision{}, fmt.Errorf("revalidate.Handle: inbound edges for %q: %w", s.FindingID, err)
		}
		if hasIn {
			// rule no longer fires — the node now has callers.
			return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil
		}
		// still dead — refresh anchor hash in place.
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionRefresh, NewHash: s.CurrentHash}, nil

	case ruleContractDrift:
		prev, cur, err := h.repo.NodeSignaturePair(ctx, row.RepoID, row.Branch, s.NodeID)
		if err != nil {
			return ports.FindingDecision{}, fmt.Errorf("revalidate.Handle: signature pair for %q: %w", s.FindingID, err)
		}
		if prev != "" && prev != cur {
			// still drifting — refresh anchor hash in place.
			return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionRefresh, NewHash: s.CurrentHash}, nil
		}
		// drift resolved (signatures match, or signature absent).
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil

	default:
		// Unknown rule: conservative close (matches m3.05.2 behaviour for
		// rules that have no defined re-run path). Note "auto-link" never
		// reaches here — see the const block above.
		return ports.FindingDecision{FindingID: s.FindingID, Kind: ports.DecisionClose}, nil
	}
}

// Compile-time check: *Handler satisfies ports.WorkHandler.
var _ ports.WorkHandler = (*Handler)(nil)
