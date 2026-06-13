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

// AC1: an exported symbol's signature change is reported as breaking, named.
func TestAPIGate_ExportedDrift_Fails(t *testing.T) {
	v := NewAPIGate().Evaluate([]ports.DriftedNode{drift("Foo", true)})
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
	v := NewAPIGate().Evaluate([]ports.DriftedNode{drift("foo", false)})
	if !v.Pass {
		t.Fatalf("unexported signature change must PASS; got %+v", v)
	}
}

// AC3 (and body-only): no drift rows at all -> PASS. The querier only returns
// genuinely-drifted nodes, so a body-only change contributes nothing here.
func TestAPIGate_NoDrift_Passes(t *testing.T) {
	v := NewAPIGate().Evaluate(nil)
	if !v.Pass {
		t.Fatalf("no drift must PASS; got %+v", v)
	}
}

// Mixed input: only the exported member is reported.
func TestAPIGate_Mixed_ReportsOnlyExported(t *testing.T) {
	v := NewAPIGate().Evaluate([]ports.DriftedNode{drift("foo", false), drift("Bar", true), drift("baz", false)})
	if v.Pass || len(v.BreakingChanges) != 1 || v.BreakingChanges[0].SymbolPath != "Bar" {
		t.Fatalf("must report only Bar; got %+v", v)
	}
}
