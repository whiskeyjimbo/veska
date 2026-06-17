// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"context"
	"fmt"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// FailClonesUnchecked names the degraded outcome where the base graph cannot
// answer content-hash membership (it does not implement the clone-lookup
// capability), so the gate cannot judge net-new duplication. Like the verify
// gate's unchecked dimensions this is a FAIL, never a PASS - a gate that
// cannot see the base must not greenlight a change.
const FailClonesUnchecked = "clones_unchecked"

// cloneHashLookup is the optional base capability the clone gate needs: how
// many existing nodes share a given content_hash, kind-filtered to match the
// whole-repo clones analyzer. The persisted graph store (sqlite.NodeLookupRepo)
// implements it; an in-memory base backend that does not leaves the gate
// degraded (Checked=false). This mirrors the nodeHasher optional capability in
// changednodes.go - structurally satisfied, type-asserted off Base.
type cloneHashLookup interface {
	NodesByContentHash(ctx context.Context, repoID, branch, hash string, excludeKinds []string) ([]ports.NodeRef, error)
}

// CloneMember is one node participating in a net-new exact-clone group.
type CloneMember struct {
	NodeID     string `json:"node_id"`
	FilePath   string `json:"file_path"`
	SymbolPath string `json:"symbol_path"`
}

// CloneGroup is a set of >=2 byte-identical nodes (shared content_hash) that
// the candidate diff newly introduced - duplication absent at base.
type CloneGroup struct {
	ContentHash string        `json:"content_hash"`
	Members     []CloneMember `json:"members"`
}

// CloneVerdict is the exact-clone diff gate's pass/fail result. Pass is true
// only when the gate was CHECKED and found no net-new clone group. Checked is
// false when the base lacks the content-hash lookup capability - a degraded
// run that fails rather than passes.
type CloneVerdict struct {
	Pass      bool         `json:"pass"`
	Checked   bool         `json:"checked"`
	NewClones []CloneGroup `json:"new_clones"`
}

// Failures returns the stable failing-check names for CI/agent consumption.
func (v CloneVerdict) Failures() []string {
	if v.Pass {
		return nil
	}
	if !v.Checked {
		return []string{FailClonesUnchecked}
	}
	return []string{"new_clone"}
}

// ExitCode is the process exit code for CI gating: 0 on PASS, 1 on FAIL.
func (v CloneVerdict) ExitCode() int {
	if v.Pass {
		return 0
	}
	return 1
}

// CloneGate flags a candidate change that introduces net-new exact-clone
// duplication: a byte-identical copy (content_hash equality) of code that the
// change did not already duplicate at base. Exact-only and embedding-free
// near-mode (thresholded SIMILAR_TO) is deliberately out, since a miscalibrated
// fuzzy gate blocks PRs.
// Net-new is decided per candidate content_hash H by comparing group size:
//
//	FAIL ⟺ afterCount(H) >= 2 AND baseCount(H) < 2
//
// where baseCount(H) is every eligible base node with hash H, and afterCount(H)
// is the after-state membership: ALL eligible overlay nodes with H (the overlay
// is the after-state of every touched file, so an unchanged twin in a co-touched
// file still counts) plus base nodes with H in files the diff did NOT touch.
// Excluding base nodes in touched files drops the stale pre-images of
// moved/modified code so a rename is not read as a new clone; the baseCount<2
// guard additionally lets a move of one member of an already-duplicated pair
// pass (the group existed at base). Adding a 3rd copy to an existing pair
// (base 2 -> after 3) PASSES: AC2 gates on a new clone GROUP, not a new member.
type CloneGate struct{}

// NewCloneGate constructs a CloneGate. It is stateless.
func NewCloneGate() *CloneGate { return &CloneGate{} }

// Evaluate runs the gate over the ephemeral candidate. It reads only - no
// durable state changes. A nil eph is a programming error and panics via the
// nil-deref; callers always pass a built Ephemeral.
func (g *CloneGate) Evaluate(ctx context.Context, eph *Ephemeral) (CloneVerdict, error) {
	lookup, ok := eph.Base.(cloneHashLookup)
	if !ok {
		// Base cannot answer content-hash membership - degraded, not a pass.
		return CloneVerdict{Pass: false, Checked: false}, nil
	}

	excluded := make(map[string]struct{}, len(duplicates.ExcludedKinds))
	for _, k := range duplicates.ExcludedKinds {
		excluded[k] = struct{}{}
	}
	touchedFiles := make(map[string]struct{}, len(eph.ChangedFiles))
	for _, f := range eph.ChangedFiles {
		touchedFiles[f] = struct{}{}
	}

	// Overlay nodes grouped by content_hash. The overlay holds the after-state
	// of every TOUCHED file, so EVERY eligible overlay node is a live member of
	// its hash group - not just the ones whose content changed. A twin that the
	// diff left unchanged but that lives in a co-touched file is still a real
	// clone member; filtering to changed-only nodes would drop it and miss a
	// net-new duplication (an AC1 false negative). Eligible = non-excluded kind,
	// non-empty hash.
	overlayByHash := make(map[string][]CloneMember)
	for _, file := range eph.Overlay.Snapshot(eph.RepoID, eph.Branch) {
		for _, n := range file.Nodes {
			if n == nil || n.ContentHash == nil {
				continue
			}
			if _, skip := excluded[string(n.Kind)]; skip {
				continue
			}
			h := string(*n.ContentHash)
			if h == "" {
				continue
			}
			overlayByHash[h] = append(overlayByHash[h], CloneMember{NodeID: string(n.ID), FilePath: n.Path, SymbolPath: n.Name})
		}
	}

	// Iterating overlay hashes is complete: a net-new group (afterCount>=2 AND
	// baseCount<2) must include >=1 overlay member, because >=2 untouched-base
	// members would themselves make baseCount>=2.
	var groups []CloneGroup
	for h, overlayMembers := range overlayByHash {
		baseNodes, err := lookup.NodesByContentHash(ctx, eph.RepoID, eph.Branch, h, duplicates.ExcludedKinds)
		if err != nil {
			return CloneVerdict{}, fmt.Errorf("clonegate: base lookup for %q: %w", h, err)
		}
		baseCount := len(baseNodes)

		// after-members = overlay nodes (after-state of touched files) ∪ base
		// nodes in UNTOUCHED files (after-state of untouched files), deduped by
		// node_id. Base nodes in touched files are excluded: the overlay already
		// carries their after-state, so counting the base pre-image too would
		// double-count and would resurrect the stale pre-image of moved code.
		members := make(map[string]CloneMember, len(overlayMembers)+baseCount)
		for _, m := range overlayMembers {
			members[m.NodeID] = m
		}
		for _, b := range baseNodes {
			if _, touched := touchedFiles[b.FilePath]; touched {
				continue
			}
			members[b.NodeID] = CloneMember{NodeID: b.NodeID, FilePath: b.FilePath, SymbolPath: b.Name}
		}

		if len(members) >= 2 && baseCount < 2 {
			groups = append(groups, CloneGroup{ContentHash: h, Members: sortedMembers(members)})
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ContentHash < groups[j].ContentHash })

	return CloneVerdict{Pass: len(groups) == 0, Checked: true, NewClones: groups}, nil
}

// sortedMembers returns the map's members ordered by (file_path, node_id) for a
// deterministic verdict.
func sortedMembers(m map[string]CloneMember) []CloneMember {
	out := make([]CloneMember, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}
