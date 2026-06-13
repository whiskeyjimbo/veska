package diffgate

import (
	"sort"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// FailUntestedChanged names the failing check: the candidate changed a prod
// symbol that no test reaches.
const FailUntestedChanged = "untested_changed_symbol"

// UntestedSymbol is one changed prod symbol the gate found has no test-file
// caller in the candidate after-state.
type UntestedSymbol struct {
	NodeID  string `json:"node_id"`
	Message string `json:"message"`
}

// CoverageVerdict is the diff-coverage gate result: PASS only when every changed
// prod symbol has a test-file caller (under the CALLS-edge proxy). It carries no
// "checked" flag — unlike the security/clone gates there is no degraded path:
// the inputs (changed set + untested findings) are always computable once the
// repo is indexed; an indexing/promote failure surfaces as an error from the
// invocation surface, not a verdict.
type CoverageVerdict struct {
	Pass            bool             `json:"pass"`
	UntestedChanged []UntestedSymbol `json:"untested_changed"`
}

// Failures returns the stable failing-check names.
func (v CoverageVerdict) Failures() []string {
	if v.Pass {
		return nil
	}
	return []string{FailUntestedChanged}
}

// ExitCode is the process exit code for CI gating: 0 on PASS, 1 on FAIL.
func (v CoverageVerdict) ExitCode() int {
	if v.Pass {
		return 0
	}
	return 1
}

// CoverageGate flags a candidate change that modifies or adds a prod symbol no
// test reaches. It is a blanket gate (no target finding). The heavy lifting —
// re-promoting the candidate so cross-file test→prod CALLS edges resolve, then
// running the untested-symbol check (solov2-zvh6.3) over the after-state — is
// the invocation surface's job; this gate only intersects the two results.
//
// The intersection is the whole gate, and it is exactly what AC2 requires:
// "untested" is judged over ALL symbols in the changed files (the check scopes
// by file), but the gate fires ONLY for symbols in the node-precision CHANGED
// set. An unchanged-but-untested symbol sharing a touched file is therefore NOT
// flagged — flagging it would fail a diff whose CHANGED symbols are all tested.
type CoverageGate struct{}

// NewCoverageGate constructs a CoverageGate. It is stateless.
func NewCoverageGate() *CoverageGate { return &CoverageGate{} }

// Evaluate intersects the untested-symbol findings (over the candidate
// after-state, scoped to the changed files) with the node-precision changed
// set. Findings anchored on a node outside the changed set are dropped; the
// rest are the changed symbols no test reaches.
func (g *CoverageGate) Evaluate(changedNodeIDs []string, untested []*domain.Finding) CoverageVerdict {
	changed := make(map[string]struct{}, len(changedNodeIDs))
	for _, id := range changedNodeIDs {
		changed[id] = struct{}{}
	}

	out := make([]UntestedSymbol, 0)
	for _, f := range untested {
		if f == nil || f.NodeID == nil {
			continue
		}
		if _, ok := changed[*f.NodeID]; !ok {
			continue
		}
		out = append(out, UntestedSymbol{NodeID: *f.NodeID, Message: f.Message})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })

	return CoverageVerdict{Pass: len(out) == 0, UntestedChanged: out}
}
