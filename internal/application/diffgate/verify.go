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
// cheaply re-run, which the gate would misread as "resolved" - a false PASS,
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
	Rule string `json:"rule"`
	// ResolutionChecked is false when the target's rule (or anchor) is not
	// soundly re-runnable over the ephemeral graph in v1; TargetResolved is
	// meaningful only when this is true.
	ResolutionChecked bool `json:"resolution_checked"`
	// TargetResolved is true when the target finding's rule no longer fires on
	// the candidate (the diff resolved it).
	TargetResolved bool `json:"target_resolved"`
	// NewFindingsChecked is false when finding-discovery over the candidate was
	// not wired; an unchecked discovery is degraded, never green.
	NewFindingsChecked bool `json:"new_findings_checked"`
	// NewFindings lists finding_ids present in the candidate but absent in the
	// base - findings the diff introduced. Sorted; empty when none (and when
	// NewFindingsChecked).
	NewFindings []string `json:"new_findings"`
	// NewFindingsCoveredRules names the rules discovery actually evaluated, so
	// a consumer never reads "checked" as "all rules checked". The no-new
	// findings result is sound ONLY for these rules; a rule absent here (e.g.
	// secrets, vuln) was NOT checked and a new finding under it would not fail
	// the gate. Empty when discovery did not run.
	NewFindingsCoveredRules []string `json:"new_findings_covered_rules"`
}

// Discovery carries the finding-id sets used for the no-new-findings check. Ran
// distinguishes "discovery was performed" from "not run": when Ran is false the
// verdict's NewFindingsChecked is false (fail-safe). BaseIDs and CandidateIDs
// are the complete sets of open finding_ids over the base and candidate graph
// states respectively - the diff is by finding identity, so ids are all the
// Verifier needs. Producing these (re-promoting changed files into a cloned
// base graph and running the real structural checks over it) is the scope:large
// adapter the gate's CLI wires; the Verifier only consumes the sets.
// SCOPE: discovery covers the graph-structural rules (dead-code, contract-drift)
// that a re-promote + full-file check pass makes sound. Line/dep scanners
// (secrets, vuln) need per-line/dep inputs and are out of v1 scope - the gate's
// "no new findings" is over structural findings.
type Discovery struct {
	Ran          bool
	BaseIDs      []string
	CandidateIDs []string
	// CoveredRules names the rules the discovery producer actually evaluated.
	// The verdict propagates it so "no new findings" is never read as covering
	// rules the producer didn't run.
	CoveredRules []string
}

// Verifier answers the verify half of the gate: did the candidate resolve its
// target finding, and did it introduce no new findings? Resolution reuses the
// revalidation rule dispatch (revalidate.Decide) re-run against the ephemeral
// graph; the Verifier owns no rule logic of its own. It is stateless and does
// no network IO.
type Verifier struct{}

// NewVerifier constructs a Verifier. It has no dependencies - the graph it
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
		out.NewFindingsCoveredRules = disc.CoveredRules
		baseIDs := make(map[string]struct{}, len(disc.BaseIDs))
		for _, id := range disc.BaseIDs {
			if id != "" {
				baseIDs[id] = struct{}{}
			}
		}
		var newF []string
		seen := make(map[string]struct{})
		for _, id := range disc.CandidateIDs {
			if id == "" {
				continue
			}
			if _, ok := baseIDs[id]; ok {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			newF = append(newF, id)
		}
		sort.Strings(newF)
		out.NewFindings = newF
	}

	return out, nil
}

// ephemeralPredicates implements revalidate.PredicateSource over the ephemeral
// graph so the dead-code re-run sees the CANDIDATE's edges, not the indexed
// base. Inbound CALLS edges = base inbound CALLS ∪ resolved overlay CALLS edges
// targeting the node. Only CALLS count: a structural CONTAINS/IMPORTS parent
// edge is not a caller, so it must not resolve a dead-code finding - counting
// it made every dead-code finding read as resolved with no fix,
// since every symbol has a CONTAINS parent. A candidate's NEW cross-file caller
// surfaces as an UnresolvedCall (bound only at promotion), not a resolved
// overlay edge, so it is NOT counted here - that under-reports inbound edges,
// which biases dead-code resolution toward "still dead / unresolved". For a
// GATE that is the safe direction (it over-blocks a genuinely-resolving change
// rather than passing an unresolved one); intra-file caller additions are
// counted exactly.
type ephemeralPredicates struct {
	eph *Ephemeral
}

func (p ephemeralPredicates) HasInboundCallEdges(ctx context.Context, repoID, branch, nodeID string) (bool, error) {
	base, err := p.eph.Base.InboundCallEdges(ctx, repoID, branch, []string{nodeID})
	if err != nil {
		return false, fmt.Errorf("diffgate: base inbound call edges for %q: %w", nodeID, err)
	}
	if len(base[nodeID]) > 0 {
		return true, nil
	}
	for _, f := range p.eph.Overlay.Snapshot(repoID, branch) {
		for _, e := range f.Edges {
			if e != nil && e.Resolved && e.Kind == domain.EdgeCalls && string(e.Tgt) == nodeID {
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

// HasTestCaller is a total-but-unused stub: untested-symbol is NOT on the
// Verifier's resolvableRules allowlist, so Decide never reaches its case via the
// gate. It returns false (= "still untested" = refresh = over-block), the
// fail-safe direction for a gate. A sound ephemeral impl would need the inbound
// edges' SRC FILE PATHS to apply the test-file predicate, which the Base
// EdgeReader port does not expose (it returns src node_ids only) - so this stays
// a stub until untested-symbol is added to the allowlist as a separate decision.
func (p ephemeralPredicates) HasTestCaller(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}
