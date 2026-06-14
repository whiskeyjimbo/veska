package diffgate

import (
	"path"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// FailBreakingAPIChange names the failing check: the candidate changed the
// signature shape of an EXPORTED symbol — a breaking public-surface change.
const FailBreakingAPIChange = "breaking_api_change"

// FailRemovedAPISymbol names the failing check: an EXPORTED symbol present at
// base-ref disappeared from the candidate (removed, renamed, or unexported) —
// arguably the most breaking public-surface change (solov2-zvh6.12).
const FailRemovedAPISymbol = "removed_api_symbol"

// APIChange is one exported symbol whose signature shape changed.
type APIChange struct {
	NodeID     string `json:"node_id"`
	FilePath   string `json:"file_path"`
	SymbolPath string `json:"symbol_path"`
	Kind       string `json:"kind"`
	PrevSig    string `json:"prev_signature"`
	NewSig     string `json:"new_signature"`
}

// APIRemoval is one exported symbol present at base-ref but absent from the
// candidate. Removal and rename collapse into this one category by design: a
// rename leaves the OLD name absent (breaking for importers), and pairing it to
// a new name has no reliable signal. NodeID/FilePath are the base-ref
// coordinates of the now-absent symbol.
type APIRemoval struct {
	NodeID     string `json:"node_id"`
	FilePath   string `json:"file_path"`
	SymbolPath string `json:"symbol_path"`
	Kind       string `json:"kind"`
}

// APIVerdict is the breaking-public-API gate's pass/fail result, covering two
// detectors: signature-shape drift of an exported symbol (BreakingChanges) and
// removal/rename of an exported symbol (RemovedSymbols). There is no degraded
// "unchecked" mode: both states are materialisable from the (cloned,
// re-promoted) base graph, so Pass == (no breaking changes AND no removals).
type APIVerdict struct {
	Pass            bool         `json:"pass"`
	BreakingChanges []APIChange  `json:"breaking_changes"`
	RemovedSymbols  []APIRemoval `json:"removed_symbols"`
}

// Failures returns the stable failing-check names for CI/agent consumption, in
// a deterministic order (drift before removal).
func (v APIVerdict) Failures() []string {
	if v.Pass {
		return nil
	}
	var f []string
	if len(v.BreakingChanges) > 0 {
		f = append(f, FailBreakingAPIChange)
	}
	if len(v.RemovedSymbols) > 0 {
		f = append(f, FailRemovedAPISymbol)
	}
	return f
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
//   - REMOVAL or RENAME of an exported symbol fires the SECOND detector
//     (RemovedSymbols, solov2-zvh6.12): a base-ref exported symbol whose
//     package-scoped identity key is absent from the candidate after-state.
//   - "Exported" is the name-based visibility flag (Go: uppercase first rune), so
//     an exported-named method on an UNEXPORTED type reads as exported and a
//     signature change there false-FAILs. A known over-approximation, not a
//     reachability analysis.
//   - The removal detector judges {function, method, interface} only (mirroring
//     contract-drift); removing an exported type/const/var is also breaking but
//     left as a documented follow-up rather than silently widening v1.
//
// Language-agnostic: signature drift judges ports.DriftedNode.Exported, and
// removal judges a package-scoped identity key (package = path.Dir(file_path)),
// both per-node flags every language's parser sets by its own visibility rule.
// The algorithm makes no Go-specific assumption beyond "directory == package",
// which holds for Go and is a reasonable default elsewhere.
type APIGate struct{}

// NewAPIGate constructs an APIGate. It is stateless.
func NewAPIGate() *APIGate { return &APIGate{} }

// symbolIdentity is the package-scoped key (package, kind, name) under which an
// exported symbol's PRESENCE is judged. package = path.Dir(file_path), so an
// intra-package file move (a.go -> b.go, same dir) keeps the key stable and is
// NOT flagged as a removal, while a cross-package move (or true removal/rename/
// unexport) changes/drops the key and IS flagged. Names are unique within a Go
// package per kind, so the key is unambiguous.
func symbolIdentity(filePath, kind, name string) string {
	return path.Dir(filePath) + "\x00" + kind + "\x00" + name
}

// Evaluate reports two classes of breaking public-API change. Pure — no I/O.
//
//   - BreakingChanges: exported nodes among drifted whose signature shape changed.
//   - RemovedSymbols: base-ref exported symbols whose package-scoped identity key
//     is absent from the candidate's exported set (removed/renamed/unexported).
//
// baseExported and candidateExported are the exported public-surface symbols
// (function/method/interface) over the CHANGED files at base-ref and in the
// candidate after-state respectively — the complete scope, since a moved
// symbol's destination is always a changed file too.
func (g *APIGate) Evaluate(drifted []ports.DriftedNode, baseExported, candidateExported []ports.ExportedSymbol) APIVerdict {
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

	candKeys := make(map[string]struct{}, len(candidateExported))
	for _, s := range candidateExported {
		candKeys[symbolIdentity(s.FilePath, s.Kind, s.Name)] = struct{}{}
	}
	var removed []APIRemoval
	for _, s := range baseExported {
		if _, present := candKeys[symbolIdentity(s.FilePath, s.Kind, s.Name)]; present {
			continue // identity survives (incl. intra-package move) — not a removal
		}
		removed = append(removed, APIRemoval{
			NodeID:     s.NodeID,
			FilePath:   s.FilePath,
			SymbolPath: s.Name,
			Kind:       s.Kind,
		})
	}
	sort.Slice(removed, func(i, j int) bool {
		if removed[i].FilePath != removed[j].FilePath {
			return removed[i].FilePath < removed[j].FilePath
		}
		return removed[i].NodeID < removed[j].NodeID
	})

	return APIVerdict{
		Pass:            len(breaking) == 0 && len(removed) == 0,
		BreakingChanges: breaking,
		RemovedSymbols:  removed,
	}
}
