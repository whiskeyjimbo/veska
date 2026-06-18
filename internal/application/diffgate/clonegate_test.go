// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgate_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeCloneBase satisfies diffgate.BaseGraph (via nil-embedded interfaces - the
// gate never calls EdgeReader/NodeLookup) plus the cloneHashLookup capability.
// It deliberately does NOT implement NodeContentHash, so ChangedNodeIDs treats
// every overlay node as changed (tests control candidacy via the overlay).
type fakeCloneBase struct {
	ports.EdgeReader
	ports.NodeLookup
	byHash map[string][]ports.NodeRef
}

func (f fakeCloneBase) NodesByContentHash(_ context.Context, _, _, hash string, _ []string) ([]ports.NodeRef, error) {
	return f.byHash[hash], nil
}

// InboundCallEdges satisfies diffgate.CallEdgeReader; the clone gate never
// re-runs dead-code resolution, so it is a no-op.
func (fakeCloneBase) InboundCallEdges(context.Context, string, string, []string) (map[string][]string, error) {
	return nil, nil
}

// plainBase satisfies BaseGraph but NOT cloneHashLookup (the degraded path).
type plainBase struct {
	ports.EdgeReader
	ports.NodeLookup
}

func (plainBase) InboundCallEdges(context.Context, string, string, []string) (map[string][]string, error) {
	return nil, nil
}

func cloneNode(t *testing.T, id, path, name, kind, hash string) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(
		domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.NodeKind(kind)},
		domain.WithContentHash(domain.ContentHash(hash)),
	)
	if err != nil {
		t.Fatalf("NewNode(%s): %v", id, err)
	}
	return n
}

func baseRef(id, path, kind, hash string) ports.NodeRef {
	return ports.NodeRef{NodeID: id, FilePath: path, Kind: kind, Name: id, ContentHash: hash}
}

// evalCase builds an Ephemeral from a base-by-hash map and a set of overlay
// files, then evaluates the gate.
func evalCase(t *testing.T, base map[string][]ports.NodeRef, repoID, branch string, changedFiles []string, overlay map[string][]*domain.Node) diffgate.CloneVerdict {
	t.Helper()
	area := staging.NewArea()
	for path, nodes := range overlay {
		area.Stage(repoID, branch, path, staging.File{Nodes: nodes})
	}
	eph := &diffgate.Ephemeral{
		Base:         fakeCloneBase{byHash: base},
		Overlay:      area,
		RepoID:       repoID,
		Branch:       branch,
		ChangedFiles: changedFiles,
	}
	v, err := diffgate.NewCloneGate().Evaluate(context.Background(), eph)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return v
}

const repo, branch = "r", "main"

// AC1: add a byte-identical copy of a unique base fn -> FAIL (base 1 -> after 2).
func TestCloneGate_AddCopyOfUniqueFn_Fails(t *testing.T) {
	base := map[string][]ports.NodeRef{"H": {baseRef("a", "a.go", "function", "H")}}
	v := evalCase(t, base, repo, branch, []string{"b.go"}, map[string][]*domain.Node{
		"b.go": {cloneNode(t, "b", "b.go", "Dup", "function", "H")},
	})
	if v.Pass || !v.Checked {
		t.Fatalf("want FAIL checked; got pass=%v checked=%v", v.Pass, v.Checked)
	}
	if len(v.NewClones) != 1 || v.NewClones[0].ContentHash != "H" || len(v.NewClones[0].Members) != 2 {
		t.Fatalf("want one H group of 2 members; got %+v", v.NewClones)
	}
}

// Modify a fn so it now matches another base fn -> FAIL (base 1 -> after 2).
func TestCloneGate_ModifyToMatch_Fails(t *testing.T) {
	base := map[string][]ports.NodeRef{"H": {baseRef("a", "a.go", "function", "H")}}
	// d.go's node now carries hash H (was something else at base).
	v := evalCase(t, base, repo, branch, []string{"d.go"}, map[string][]*domain.Node{
		"d.go": {cloneNode(t, "d", "d.go", "Drifted", "function", "H")},
	})
	if v.Pass {
		t.Fatalf("modify-to-match should FAIL; got %+v", v)
	}
}

// AC2: modify a fn to a unique hash -> PASS (no match).
func TestCloneGate_ModifyNoMatch_Passes(t *testing.T) {
	base := map[string][]ports.NodeRef{} // nothing shares K
	v := evalCase(t, base, repo, branch, []string{"d.go"}, map[string][]*domain.Node{
		"d.go": {cloneNode(t, "d", "d.go", "Unique", "function", "K")},
	})
	if !v.Pass || !v.Checked {
		t.Fatalf("modify-no-match should PASS; got %+v", v)
	}
}

// Rename/move a unique fn -> PASS (base 1 -> after 1; pre-image in touched file
// excluded).
func TestCloneGate_RenameUniqueFn_Passes(t *testing.T) {
	// base node "a" lives in old.go (which the diff deletes/moves from).
	base := map[string][]ports.NodeRef{"H": {baseRef("a", "old.go", "function", "H")}}
	v := evalCase(t, base, repo, branch, []string{"old.go", "new.go"}, map[string][]*domain.Node{
		"new.go": {cloneNode(t, "a-moved", "new.go", "Moved", "function", "H")},
	})
	if !v.Pass {
		t.Fatalf("rename of a unique fn should PASS; got %+v", v)
	}
}

// Move one member of an existing clone pair -> PASS (base 2 -> after 2).
func TestCloneGate_MoveMemberOfExistingPair_Passes(t *testing.T) {
	base := map[string][]ports.NodeRef{"H": {
		baseRef("a", "old.go", "function", "H"),
		baseRef("b", "keep.go", "function", "H"),
	}}
	v := evalCase(t, base, repo, branch, []string{"old.go", "new.go"}, map[string][]*domain.Node{
		"new.go": {cloneNode(t, "a-moved", "new.go", "Moved", "function", "H")},
	})
	if !v.Pass {
		t.Fatalf("moving one member of an existing pair should PASS; got %+v", v)
	}
}

// Two new identical fns in one diff -> FAIL (base 0 -> after 2).
func TestCloneGate_TwoNewIdentical_Fails(t *testing.T) {
	base := map[string][]ports.NodeRef{}
	v := evalCase(t, base, repo, branch, []string{"x.go", "y.go"}, map[string][]*domain.Node{
		"x.go": {cloneNode(t, "x", "x.go", "Foo", "function", "H")},
		"y.go": {cloneNode(t, "y", "y.go", "Bar", "function", "H")},
	})
	if v.Pass {
		t.Fatalf("two new identical fns should FAIL; got %+v", v)
	}
	if len(v.NewClones) != 1 || len(v.NewClones[0].Members) != 2 {
		t.Fatalf("want one group of 2; got %+v", v.NewClones)
	}
}

// Fork (decided PASS): adding a 3rd copy to an existing pair (base 2 -> after 3)
// is a new MEMBER, not a new GROUP. AC2 binds -> PASS.
func TestCloneGate_ThirdCopyToExistingPair_Passes(t *testing.T) {
	base := map[string][]ports.NodeRef{"H": {
		baseRef("a", "a.go", "function", "H"),
		baseRef("b", "b.go", "function", "H"),
	}}
	v := evalCase(t, base, repo, branch, []string{"c.go"}, map[string][]*domain.Node{
		"c.go": {cloneNode(t, "c", "c.go", "Third", "function", "H")},
	})
	if !v.Pass {
		t.Fatalf("3rd copy to an existing pair should PASS (new member, not new group); got %+v", v)
	}
}

// Co-touched twin (AC1 false-negative regression): the clone's twin is
// UNCHANGED but lives in a file the same diff also touches. The overlay is the
// after-state of touched files, so the unchanged twin is a live member - the
// gate must still FAIL (base 1 -> after 2).
func TestCloneGate_CoTouchedUnchangedTwin_Fails(t *testing.T) {
	// base has Must only in a/util.go (unique).
	base := map[string][]ports.NodeRef{"H": {baseRef("must-a", "a/util.go", "function", "H")}}
	// Diff touches a/util.go (Must unchanged -> reparsed into the overlay) and
	// adds an identical Must in the co-touched b/util.go.
	v := evalCase(t, base, repo, branch, []string{"a/util.go", "b/util.go"}, map[string][]*domain.Node{
		"a/util.go": {cloneNode(t, "must-a", "a/util.go", "Must", "function", "H")},
		"b/util.go": {cloneNode(t, "must-b", "b/util.go", "Must", "function", "H")},
	})
	if v.Pass {
		t.Fatalf("co-touched unchanged twin should FAIL; got %+v", v)
	}
	if len(v.NewClones) != 1 || len(v.NewClones[0].Members) != 2 {
		t.Fatalf("want one group of 2 (a/util.go + b/util.go); got %+v", v.NewClones)
	}
}

// AC3: excluded kinds (field/import/etc.) never trip the gate.
func TestCloneGate_ExcludedKinds_Pass(t *testing.T) {
	base := map[string][]ports.NodeRef{"H": {baseRef("a", "a.go", "field", "H")}}
	v := evalCase(t, base, repo, branch, []string{"b.go"}, map[string][]*domain.Node{
		"b.go": {cloneNode(t, "b", "b.go", "Field", "field", "H")},
	})
	if !v.Pass {
		t.Fatalf("excluded-kind duplication should PASS; got %+v", v)
	}
}

// Degraded: a base without the lookup capability is Checked=false, Pass=false.
func TestCloneGate_DegradedWhenBaseLacksLookup(t *testing.T) {
	area := staging.NewArea()
	area.Stage(repo, branch, "b.go", staging.File{Nodes: []*domain.Node{
		cloneNode(t, "b", "b.go", "Dup", "function", "H"),
	}})
	eph := &diffgate.Ephemeral{
		Base:         plainBase{},
		Overlay:      area,
		RepoID:       repo,
		Branch:       branch,
		ChangedFiles: []string{"b.go"},
	}
	v, err := diffgate.NewCloneGate().Evaluate(context.Background(), eph)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || v.Checked {
		t.Fatalf("degraded base should be FAIL+unchecked; got pass=%v checked=%v", v.Pass, v.Checked)
	}
	if v.ExitCode() != 1 || len(v.Failures()) != 1 || v.Failures()[0] != diffgate.FailClonesUnchecked {
		t.Fatalf("degraded failures wrong: exit=%d failures=%v", v.ExitCode(), v.Failures())
	}
}

func TestCloneVerdict_ExitCode(t *testing.T) {
	if (diffgate.CloneVerdict{Pass: true}).ExitCode() != 0 {
		t.Error("pass exit != 0")
	}
	if (diffgate.CloneVerdict{Pass: false}).ExitCode() != 1 {
		t.Error("fail exit != 1")
	}
}
