// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"reflect"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

func drift(name string, exported bool) ports.DriftedNode {
	return ports.DriftedNode{
		NodeID:   "id:" + name,
		FilePath: "x.go",
		Kind:     "function",
		Name:     name,
		PrevSig:  name + "(a int)",
		NewSig:   name + "(a, b int)",
		Exported: exported,
	}
}

func exp(file, name string) ports.ExportedSymbol {
	return ports.ExportedSymbol{NodeID: "id:" + file + ":" + name, FilePath: file, Kind: "function", Name: name}
}

// AC1: an exported symbol's signature change is reported as breaking, named.
func TestAPIGate_ExportedDrift_Fails(t *testing.T) {
	v := NewAPIGate().Evaluate([]ports.DriftedNode{drift("Foo", true)}, nil, nil)
	if v.Pass {
		t.Fatalf("exported signature change must FAIL; got %+v", v)
	}
	if len(v.BreakingChanges) != 1 || v.BreakingChanges[0].SymbolPath != "Foo" {
		t.Fatalf("must name Foo; got %+v", v.BreakingChanges)
	}
	if !reflect.DeepEqual(v.Failures(), []string{FailBreakingAPIChange}) {
		t.Fatalf("failures = %v", v.Failures())
	}
}

// AC2: an unexported symbol's signature change passes (filtered out).
func TestAPIGate_UnexportedDrift_Passes(t *testing.T) {
	v := NewAPIGate().Evaluate([]ports.DriftedNode{drift("foo", false)}, nil, nil)
	if !v.Pass {
		t.Fatalf("unexported signature change must PASS; got %+v", v)
	}
}

// AC3 (and body-only): no drift rows at all -> PASS. The querier only returns
// genuinely-drifted nodes, so a body-only change contributes nothing here.
func TestAPIGate_NoDrift_Passes(t *testing.T) {
	v := NewAPIGate().Evaluate(nil, nil, nil)
	if !v.Pass {
		t.Fatalf("no drift must PASS; got %+v", v)
	}
}

// Mixed input: only the exported member is reported.
func TestAPIGate_Mixed_ReportsOnlyExported(t *testing.T) {
	v := NewAPIGate().Evaluate([]ports.DriftedNode{drift("foo", false), drift("Bar", true), drift("baz", false)}, nil, nil)
	if v.Pass || len(v.BreakingChanges) != 1 || v.BreakingChanges[0].SymbolPath != "Bar" {
		t.Fatalf("must report only Bar; got %+v", v)
	}
}

// Removal: a base-ref exported symbol absent from the candidate FAILs, named.
func TestAPIGate_Removal_Fails(t *testing.T) {
	base := []ports.ExportedSymbol{exp("a.go", "Foo"), exp("a.go", "Bar")}
	cand := []ports.ExportedSymbol{exp("a.go", "Foo")} // Bar gone
	v := NewAPIGate().Evaluate(nil, base, cand)
	if v.Pass || len(v.RemovedSymbols) != 1 || v.RemovedSymbols[0].SymbolPath != "Bar" {
		t.Fatalf("must report Bar removed; got %+v", v)
	}
	if !reflect.DeepEqual(v.Failures(), []string{FailRemovedAPISymbol}) {
		t.Fatalf("failures = %v", v.Failures())
	}
}

// The discriminating test (proves the package-scoped identity key, not node_id):
// moving an exported symbol to another file in the SAME package must PASS.
func TestAPIGate_IntraPackageMove_Passes(t *testing.T) {
	base := []ports.ExportedSymbol{exp("p/a.go", "Foo")}
	cand := []ports.ExportedSymbol{exp("p/b.go", "Foo")} // same dir, different file
	v := NewAPIGate().Evaluate(nil, base, cand)
	if !v.Pass {
		t.Fatalf("intra-package move must PASS (identity = pkg+kind+name); got %+v", v)
	}
}

// Cross-package move IS breaking: the base package no longer exports Foo.
func TestAPIGate_CrossPackageMove_Fails(t *testing.T) {
	base := []ports.ExportedSymbol{exp("p/a.go", "Foo")}
	cand := []ports.ExportedSymbol{exp("q/a.go", "Foo")} // moved to package q
	v := NewAPIGate().Evaluate(nil, base, cand)
	if v.Pass || len(v.RemovedSymbols) != 1 || v.RemovedSymbols[0].SymbolPath != "Foo" {
		t.Fatalf("cross-package move must FAIL; got %+v", v)
	}
}
