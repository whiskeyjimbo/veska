// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgate_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func untestedFinding(t *testing.T, nodeID string) *domain.Finding {
	t.Helper()
	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID: "r", Branch: "main", Severity: domain.SeverityLow,
		Layer: domain.LayerStructural, Rule: "untested-symbol",
		Message: "symbol " + nodeID + " has no test-file caller",
	}, domain.WithNodeAnchor(nodeID))
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	return f
}

// AC1: a changed symbol that is untested -> FAIL, listing it.
func TestCoverageGate_ChangedAndUntested_Fails(t *testing.T) {
	g := diffgate.NewCoverageGate()
	v := g.Evaluate([]string{"n1", "n2"}, []*domain.Finding{untestedFinding(t, "n1")})
	if v.Pass {
		t.Fatalf("want FAIL; got %+v", v)
	}
	if len(v.UntestedChanged) != 1 || v.UntestedChanged[0].NodeID != "n1" {
		t.Fatalf("want [n1]; got %+v", v.UntestedChanged)
	}
	if got := v.Failures(); len(got) != 1 || got[0] != diffgate.FailUntestedChanged {
		t.Fatalf("failures = %v", got)
	}
	if v.ExitCode() != 1 {
		t.Fatalf("exit = %d, want 1", v.ExitCode())
	}
}

// AC2: an untested symbol that is NOT in the changed set (unchanged symbol
// sharing a touched file) must NOT fail the gate - the changed symbols are all
// tested.
func TestCoverageGate_UntestedButUnchanged_Passes(t *testing.T) {
	g := diffgate.NewCoverageGate()
	// n2 is untested but the changed set is {n1}; n1 has no untested finding.
	v := g.Evaluate([]string{"n1"}, []*domain.Finding{untestedFinding(t, "n2")})
	if !v.Pass {
		t.Fatalf("untested-but-unchanged must PASS; got %+v", v)
	}
}

// AC2: changed symbols all tested (no untested findings) -> PASS.
func TestCoverageGate_AllTested_Passes(t *testing.T) {
	g := diffgate.NewCoverageGate()
	v := g.Evaluate([]string{"n1", "n2"}, nil)
	if !v.Pass || v.ExitCode() != 0 {
		t.Fatalf("all-tested must PASS; got %+v", v)
	}
}

// Multiple untested changed symbols are all listed, sorted.
func TestCoverageGate_MultipleUntested_Listed(t *testing.T) {
	g := diffgate.NewCoverageGate()
	v := g.Evaluate([]string{"n3", "n1", "n2"}, []*domain.Finding{
		untestedFinding(t, "n3"), untestedFinding(t, "n1"),
	})
	if v.Pass || len(v.UntestedChanged) != 2 {
		t.Fatalf("want 2 untested; got %+v", v)
	}
	if v.UntestedChanged[0].NodeID != "n1" || v.UntestedChanged[1].NodeID != "n3" {
		t.Fatalf("want sorted [n1 n3]; got %+v", v.UntestedChanged)
	}
}

// A file-anchored finding (no node anchor) is ignored - the gate is node-keyed.
func TestCoverageGate_FileAnchoredIgnored(t *testing.T) {
	g := diffgate.NewCoverageGate()
	f, err := domain.NewFinding(domain.FindingSpec{
		RepoID: "r", Branch: "main", Severity: domain.SeverityLow,
		Layer: domain.LayerStructural, Rule: "untested-symbol", Message: "file",
	}, domain.WithFileAnchor("a.go"))
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	v := g.Evaluate([]string{"n1"}, []*domain.Finding{f})
	if !v.Pass {
		t.Fatalf("file-anchored finding must not fire the node-keyed gate; got %+v", v)
	}
}
