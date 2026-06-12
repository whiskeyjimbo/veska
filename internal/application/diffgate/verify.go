package diffgate

import (
	"context"
	"fmt"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// resolvableRules is the v1 allowlist of rules whose resolution can be re-run
// SOUNDLY over the ephemeral graph using only the Base ports + overlay edges.
// It MUST be consulted before delegating to revalidate.Decide: Decide's
// default case returns DecisionClose ("rule obsolete") for any rule it can't
// cheaply re-run, which the gate would misread as "resolved" — a false PASS,
// the one unsafe direction. dead-code is sound now (edge-based). contract-drift
// needs node-signature access the Base ports don't expose; it joins the
// allowlist when the ephemeral finding-discovery adapter lands (follow-up).
var resolvableRules = map[string]struct{}{
	"dead-code": {},
}

// VerifyVerdict is the resolved/no-new-findings half of the diff-safety gate.
// It is two independent sub-verdicts, each carrying a "checked" flag so the
// composer (ll57.2) can distinguish "checked, clean" from "not checked" and
// NEVER read an unchecked dimension as PASS. The Verifier emits the verdict; it
// never merges or blocks.
type VerifyVerdict struct {
	Rule string
	// ResolutionChecked is false when the target's rule (or anchor) is not
	// soundly re-runnable over the ephemeral graph in v1; TargetResolved is
	// meaningful only when this is true.
	ResolutionChecked bool
	// TargetResolved is true when the target finding's rule no longer fires on
	// the candidate (the diff resolved it).
	TargetResolved bool
	// NewFindingsChecked is false when finding-discovery over the candidate was
	// not wired; an unchecked discovery is degraded, never green.
	NewFindingsChecked bool
	// NewFindings lists finding_ids present in the candidate but absent in the
	// base — findings the diff introduced. Sorted; empty when none (and when
	// NewFindingsChecked).
	NewFindings []string
}

// Discovery carries the finding sets used for the no-new-findings check. Ran
// distinguishes "discovery was performed" from "no findings found": when Ran is
// false the verdict's NewFindingsChecked is false (fail-safe). Base and
// Candidate are the complete finding sets over the base and candidate graph
// states respectively; the producer of these sets (running the structural
// checks over an ephemeral-backed querier) is the scope:large adapter deferred
// to a follow-up — the Verifier consumes them, it does not produce them.
type Discovery struct {
	Ran       bool
	Base      []*domain.Finding
	Candidate []*domain.Finding
}

// Verifier answers the verify half of the gate: did the candidate resolve its
// target finding, and did it introduce no new findings? Resolution reuses the
// revalidation rule dispatch (revalidate.Decide) re-run against the ephemeral
// graph; the Verifier owns no rule logic of its own. It is stateless and does
// no network IO.
type Verifier struct{}

// NewVerifier constructs a Verifier. It has no dependencies — the graph it
// reads is supplied per-call via the Ephemeral.
func NewVerifier() *Verifier { return &Verifier{} }

// Verify computes the resolution and no-new-findings sub-verdicts for target
// over the ephemeral graph. target must be node-anchored and its rule must be
// on the v1 resolvable allowlist for the resolution sub-verdict to be checked;
// otherwise ResolutionChecked is false and the composer degrades. disc supplies
// the finding sets for the no-new-findings diff.
func (v *Verifier) Verify(ctx context.Context, eph *Ephemeral, target *domain.Finding, disc Discovery) (VerifyVerdict, error) {
	if eph == nil {
		return VerifyVerdict{}, fmt.Errorf("%w: ephemeral graph is nil", ErrMissingDependency)
	}
	if target == nil {
		return VerifyVerdict{}, fmt.Errorf("%w: target finding is nil", ErrMissingDependency)
	}
	out := VerifyVerdict{Rule: target.Rule}

	// Resolution: only re-run via Decide when the rule is soundly supported
	// AND the finding is node-anchored. Gating BEFORE Decide is what prevents
	// Decide's default "close" from being misread as a false "resolved".
	_, supported := resolvableRules[target.Rule]
	if supported && target.NodeID != nil {
		sf := ports.StaleFinding{
			FindingID: target.FindingID,
			NodeID:    *target.NodeID,
			Rule:      target.Rule,
		}
		decision, err := revalidate.Decide(ctx, eph.RepoID, eph.Branch, sf, ephemeralPredicates{eph: eph})
		if err != nil {
			return VerifyVerdict{}, fmt.Errorf("diffgate: verify resolution for %q: %w", target.FindingID, err)
		}
		out.ResolutionChecked = true
		// DecisionClose == rule no longer fires == resolved.
		out.TargetResolved = decision.Kind == ports.DecisionClose
	}

	// No-new-findings: a set difference over finding_id. Unrun discovery is
	// degraded, not green.
	if disc.Ran {
		out.NewFindingsChecked = true
		baseIDs := make(map[string]struct{}, len(disc.Base))
		for _, f := range disc.Base {
			if f != nil {
				baseIDs[f.FindingID] = struct{}{}
			}
		}
		var newF []string
		seen := make(map[string]struct{})
		for _, f := range disc.Candidate {
			if f == nil {
				continue
			}
			if _, ok := baseIDs[f.FindingID]; ok {
				continue
			}
			if _, dup := seen[f.FindingID]; dup {
				continue
			}
			seen[f.FindingID] = struct{}{}
			newF = append(newF, f.FindingID)
		}
		sort.Strings(newF)
		out.NewFindings = newF
	}

	return out, nil
}

// ephemeralPredicates implements revalidate.PredicateSource over the ephemeral
// graph so the dead-code re-run sees the CANDIDATE's edges, not the indexed
// base. Inbound edges = base inbound ∪ resolved overlay edges targeting the
// node. A candidate's NEW cross-file caller surfaces as an UnresolvedCall
// (bound only at promotion), not a resolved overlay edge, so it is NOT counted
// here — that under-reports inbound edges, which biases dead-code resolution
// toward "still dead / unresolved". For a GATE that is the safe direction (it
// over-blocks a genuinely-resolving change rather than passing an unresolved
// one); intra-file caller additions are counted exactly.
type ephemeralPredicates struct {
	eph *Ephemeral
}

func (p ephemeralPredicates) HasInboundEdges(ctx context.Context, repoID, branch, nodeID string) (bool, error) {
	base, err := p.eph.Base.InboundEdges(ctx, repoID, branch, []string{nodeID})
	if err != nil {
		return false, fmt.Errorf("diffgate: base inbound edges for %q: %w", nodeID, err)
	}
	if len(base[nodeID]) > 0 {
		return true, nil
	}
	for _, f := range p.eph.Overlay.Snapshot(repoID, branch) {
		for _, e := range f.Edges {
			if e != nil && e.Resolved && string(e.Tgt) == nodeID {
				return true, nil
			}
		}
	}
	return false, nil
}

// NodeSignaturePair is a total-but-unused stub: the Verifier's rule allowlist
// excludes contract-drift in v1, so Decide never reaches this for a real
// decision. It returns the "drift resolved" zero. Implementing it soundly needs
// node-signature access the Base ports don't expose (follow-up).
func (p ephemeralPredicates) NodeSignaturePair(_ context.Context, _, _, _ string) (string, string, error) {
	return "", "", nil
}
