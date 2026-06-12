package diffgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// Named failing checks reported in GateVerdict.Failures. Stable strings: CI
// and the do-er match on them.
const (
	// FailUnresolved: the target finding's rule still fires on the candidate.
	FailUnresolved = "unresolved"
	// FailResolutionUnchecked: the target's rule/anchor is not soundly
	// re-runnable over the ephemeral graph in v1 — degraded, not a pass.
	FailResolutionUnchecked = "resolution_unchecked"
	// FailNewFindings: the candidate introduced finding(s) absent in the base.
	FailNewFindings = "new_findings"
	// FailDiscoveryUnchecked: no-new-findings discovery was not run — degraded,
	// not a pass (the fail-safe that stops a stubbed discovery from greenlighting
	// exactly the changes the gate exists to catch).
	FailDiscoveryUnchecked = "discovery_unchecked"
	// FailBlastRadiusExceeded: the candidate touched nodes outside the finding's
	// blast radius.
	FailBlastRadiusExceeded = "blast_radius_exceeded"
	// FailAnchorNotResolved: the target finding's anchor node does not resolve
	// in the base graph, so its blast radius — and thus scope containment —
	// cannot be computed. Degraded, not a pass (fail-safe).
	FailAnchorNotResolved = "anchor_not_resolved"
)

// GateVerdict is the single machine-readable pass/fail result over a candidate
// change. Pass is true only when every constituent check was both CHECKED and
// clean; any unchecked dimension fails (fail-safe). Failures names the failing
// checks for CI/agent consumption. The two sub-verdicts are embedded for
// explainability. The Gate emits this; it never merges or blocks.
type GateVerdict struct {
	Pass     bool          `json:"pass"`
	Failures []string      `json:"failures"`
	Verify   VerifyVerdict `json:"verify"`
	Scope    ScopeVerdict  `json:"scope"`
}

// ExitCode is the process exit code for CI gating: 0 on PASS, 1 on FAIL.
func (v GateVerdict) ExitCode() int {
	if v.Pass {
		return 0
	}
	return 1
}

// derefAnchor returns the finding's node anchor, or "" for a file-anchored
// finding.
func derefAnchor(f *domain.Finding) string {
	if f.NodeID == nil {
		return ""
	}
	return *f.NodeID
}

// Gate composes the verify and scope-containment sub-verdicts into one pass/fail
// result. It adds no analysis logic of its own — all judgement lives in the
// Verifier and Guard; the Gate only runs both and combines their outputs.
type Gate struct {
	verifier *Verifier
	guard    *Guard
}

// NewGate constructs a Gate over a Verifier and Guard. Both are required.
func NewGate(verifier *Verifier, guard *Guard) (*Gate, error) {
	if verifier == nil {
		return nil, fmt.Errorf("%w: verifier is nil", ErrMissingDependency)
	}
	if guard == nil {
		return nil, fmt.Errorf("%w: guard is nil", ErrMissingDependency)
	}
	return &Gate{verifier: verifier, guard: guard}, nil
}

// Evaluate runs verify + guard over the ephemeral candidate and returns the
// composed verdict. target is the finding the change claims to resolve; disc
// supplies the no-new-findings finding sets; radiusOpts selects the
// blast-radius policy (depth/direction/bounds) the scope check uses.
//
// PASS requires ALL of: resolution checked AND resolved; discovery checked AND
// no new findings; blast radius contained. A node-anchored target is required
// for the scope check — a file-anchored target degrades to resolution_unchecked
// and is not scope-checked.
func (g *Gate) Evaluate(ctx context.Context, eph *Ephemeral, target *domain.Finding, disc Discovery, radiusOpts blastradius.Options) (GateVerdict, error) {
	if eph == nil {
		return GateVerdict{}, fmt.Errorf("%w: ephemeral graph is nil", ErrMissingDependency)
	}
	if target == nil {
		return GateVerdict{}, fmt.Errorf("%w: target finding is nil", ErrMissingDependency)
	}

	vv, err := g.verifier.Verify(ctx, eph, target, disc)
	if err != nil {
		return GateVerdict{}, fmt.Errorf("diffgate: verify: %w", err)
	}

	var failures []string
	switch {
	case !vv.ResolutionChecked:
		failures = append(failures, FailResolutionUnchecked)
	case !vv.TargetResolved:
		failures = append(failures, FailUnresolved)
	}
	switch {
	case !vv.NewFindingsChecked:
		failures = append(failures, FailDiscoveryUnchecked)
	case len(vv.NewFindings) > 0:
		failures = append(failures, FailNewFindings)
	}

	// Scope check needs a node anchor; a file-anchored target is already
	// flagged resolution_unchecked above, so skip the guard rather than error.
	sv := ScopeVerdict{AnchorNodeID: derefAnchor(target)}
	if target.NodeID != nil {
		sv, err = g.guard.Check(ctx, eph, *target.NodeID, radiusOpts)
		switch {
		case errors.Is(err, blastradius.ErrSeedNotFound):
			// Anchor not in the base graph → radius undefined → can't assess
			// scope. Degrade to a named failure rather than crashing.
			sv = ScopeVerdict{AnchorNodeID: *target.NodeID}
			failures = append(failures, FailAnchorNotResolved)
		case err != nil:
			return GateVerdict{}, fmt.Errorf("diffgate: guard: %w", err)
		case !sv.Contained:
			failures = append(failures, FailBlastRadiusExceeded)
		}
	}

	return GateVerdict{
		Pass:     len(failures) == 0,
		Failures: failures,
		Verify:   vv,
		Scope:    sv,
	}, nil
}
