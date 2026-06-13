package diffgate

import (
	"sort"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// FailBreakingAPIChange names the failing check: the candidate changed the
// signature shape of an EXPORTED symbol — a breaking public-surface change.
const FailBreakingAPIChange = "breaking_api_change"

// APIChange is one exported symbol whose signature shape changed.
type APIChange struct {
	NodeID     string `json:"node_id"`
	FilePath   string `json:"file_path"`
	SymbolPath string `json:"symbol_path"`
	Kind       string `json:"kind"`
	PrevSig    string `json:"prev_signature"`
	NewSig     string `json:"new_signature"`
}

// APIVerdict is the breaking-exported-signature gate's pass/fail result. There
// is no degraded "unchecked" mode: the after-state signatures are always
// materialisable from the (cloned, re-promoted) base graph, so
// Pass == (len(BreakingChanges) == 0).
type APIVerdict struct {
	Pass            bool        `json:"pass"`
	BreakingChanges []APIChange `json:"breaking_changes"`
}

// Failures returns the stable failing-check name for CI/agent consumption.
func (v APIVerdict) Failures() []string {
	if v.Pass {
		return nil
	}
	return []string{FailBreakingAPIChange}
}

// ExitCode is the process exit code for CI gating: 0 on PASS, 1 on FAIL.
func (v APIVerdict) ExitCode() int {
	if v.Pass {
		return 0
	}
	return 1
}

// APIGate flags a candidate change that alters the signature SHAPE of an
// EXPORTED symbol — a breaking change a reviewer misses on a large diff. It
// reuses the whole-repo contract-drift signal (a node whose prev_signature
// differs from its signature after the candidate is re-promoted) and applies the
// public-surface policy here: only nodes with Exported==true fail. So an
// unexported signature change PASSES (AC2), and a body-only change PASSES (AC3,
// no drift row — prev_sig == sig). Drift is self-scoping (it only fires where a
// signature genuinely changed in this re-promote), so no change-set intersection
// is needed.
//
// Scope (deliberate, matching the bead's "exported symbols only / signature
// shape" boundary; see also the family-wide index-ahead caveat zvh6.11):
//   - Signature shape is the parser's signature string (name + parameters +
//     result). It includes parameter NAMES, so a cosmetic parameter rename of an
//     exported symbol false-FAILs — documented, not judged for semantic breakage.
//   - REMOVAL or RENAME of an exported symbol does NOT fire: the node is
//     delete-replaced, so prev_sig != sig never evaluates. Removal detection is a
//     separate follow-up (solov2-zvh6.12).
//   - "Exported" is the name-based visibility flag (Go: uppercase first rune), so
//     an exported-named method on an UNEXPORTED type reads as exported and a
//     signature change there false-FAILs. A known over-approximation, not a
//     reachability analysis.
//
// Language-agnostic: it judges ports.DriftedNode.Exported, a per-node flag every
// language's parser sets by its own visibility rule (Go uppercase, and others as
// added). The algorithm makes no Go-specific assumption.
type APIGate struct{}

// NewAPIGate constructs an APIGate. It is stateless.
func NewAPIGate() *APIGate { return &APIGate{} }

// Evaluate filters the re-promoted candidate's drifted nodes to the exported
// ones and reports them as breaking changes. Pure — no I/O.
func (g *APIGate) Evaluate(drifted []ports.DriftedNode) APIVerdict {
	var breaking []APIChange
	for _, d := range drifted {
		if !d.Exported {
			continue // unexported signature change is not a public-API break (AC2)
		}
		breaking = append(breaking, APIChange{
			NodeID:     d.NodeID,
			FilePath:   d.FilePath,
			SymbolPath: d.Name,
			Kind:       d.Kind,
			PrevSig:    d.PrevSig,
			NewSig:     d.NewSig,
		})
	}
	sort.Slice(breaking, func(i, j int) bool {
		if breaking[i].FilePath != breaking[j].FilePath {
			return breaking[i].FilePath < breaking[j].FilePath
		}
		return breaking[i].NodeID < breaking[j].NodeID
	})
	return APIVerdict{Pass: len(breaking) == 0, BreakingChanges: breaking}
}
