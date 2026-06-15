package coverage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/coverage"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeReader is an in-memory InboundCallsReader. inbound[id] is the list of
// nodes whose inbound CALLS edges point at id (i.e. id's callers).
type fakeReader struct {
	inbound map[string][]ports.NodeRef
}

func (f *fakeReader) InboundCallsEdges(_ context.Context, _, _ string, nodeIDs []string) (map[string][]ports.NodeRef, error) {
	out := make(map[string][]ports.NodeRef, len(nodeIDs))
	for _, id := range nodeIDs {
		out[id] = f.inbound[id]
	}
	return out, nil
}

func fn(id, name, file string) ports.NodeRef {
	return ports.NodeRef{NodeID: id, Name: name, FilePath: file, Kind: "function"}
}

func mustMap(t *testing.T, r coverage.InboundCallsReader, opts ...coverage.ReverseMapOption) *coverage.ReverseMap {
	t.Helper()
	m, err := coverage.NewReverseMap(r, opts...)
	if err != nil {
		t.Fatalf("NewReverseMap: %v", err)
	}
	return m
}

func names(tests []coverage.TestRef) []string {
	out := make([]string, len(tests))
	for i, tr := range tests {
		out[i] = tr.Name
	}
	return out
}

// TestDirectTestCaller: a prod node called directly by a test function.
func TestDirectTestCaller(t *testing.T) {
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"prod": {fn("t1", "TestProd", "prod_test.go")},
	}}
	got, err := mustMap(t, r).TestsCovering(context.Background(), "repo", "main", "prod")
	if err != nil {
		t.Fatalf("TestsCovering: %v", err)
	}
	if len(got) != 1 || got[0].Name != "TestProd" {
		t.Fatalf("got %v, want [TestProd]", names(got))
	}
}

// TestTransitiveThroughHelper: prod ← helper ← TestX. The helper is walked
// THROUGH (not an endpoint); only the test entrypoint is collected.
func TestTransitiveThroughHelper(t *testing.T) {
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"prod":   {fn("helper", "buildFixture", "helpers_test.go")},
		"helper": {fn("t1", "TestViaHelper", "prod_test.go")},
	}}
	got, err := mustMap(t, r).TestsCovering(context.Background(), "repo", "main", "prod")
	if err != nil {
		t.Fatalf("TestsCovering: %v", err)
	}
	if len(got) != 1 || got[0].Name != "TestViaHelper" {
		t.Fatalf("got %v, want [TestViaHelper]; helper must not be an endpoint", names(got))
	}
}

// TestUncoveredReturnsEmpty: a node with no test caller (only prod callers, or
// none) returns an empty set, not an error (AC2).
func TestUncoveredReturnsEmpty(t *testing.T) {
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"prod":   {fn("caller", "OtherProd", "other.go")},
		"caller": nil,
	}}
	got, err := mustMap(t, r).TestsCovering(context.Background(), "repo", "main", "prod")
	if err != nil {
		t.Fatalf("TestsCovering: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", names(got))
	}
}

// TestUnionAndDedup: TestsCoveringAny unions across seeds and de-duplicates a
// test reached from more than one seed.
func TestUnionAndDedup(t *testing.T) {
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"a": {fn("shared", "TestShared", "a_test.go"), fn("ta", "TestA", "a_test.go")},
		"b": {fn("shared", "TestShared", "a_test.go"), fn("tb", "TestB", "b_test.go")},
	}}
	got, err := mustMap(t, r).TestsCoveringAny(context.Background(), "repo", "main", []string{"a", "b"})
	if err != nil {
		t.Fatalf("TestsCoveringAny: %v", err)
	}
	// TestShared appears once despite covering both seeds.
	want := map[string]int{"TestShared": 0, "TestA": 0, "TestB": 0}
	for _, n := range names(got) {
		want[n]++
	}
	if len(got) != 3 || want["TestShared"] != 1 || want["TestA"] != 1 || want["TestB"] != 1 {
		t.Fatalf("got %v, want exactly [TestShared TestA TestB] de-duplicated", names(got))
	}
}

// TestNonTestCallerNotEndpoint: a function in a *_test.go file whose name is
// not a go-test prefix (a helper) is not collected even when it directly calls
// the prod node — it is walked through to whatever calls IT.
func TestHelperInTestFileNotEndpoint(t *testing.T) {
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"prod": {fn("h", "setupGraph", "x_test.go")}, // helper, no test above it
	}}
	got, err := mustMap(t, r).TestsCovering(context.Background(), "repo", "main", "prod")
	if err != nil {
		t.Fatalf("TestsCovering: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (setupGraph is a helper, not an entrypoint)", names(got))
	}
}

// TestCyclesTerminate: a cycle in the call graph must not loop forever.
func TestCyclesTerminate(t *testing.T) {
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"a": {fn("b", "helperB", "b_test.go")},
		"b": {fn("a", "helperA", "a_test.go"), fn("t", "TestCycle", "c_test.go")},
	}}
	got, err := mustMap(t, r).TestsCovering(context.Background(), "repo", "main", "a")
	if err != nil {
		t.Fatalf("TestsCovering: %v", err)
	}
	if len(got) != 1 || got[0].Name != "TestCycle" {
		t.Fatalf("got %v, want [TestCycle] (cycle must terminate)", names(got))
	}
}

// TestMaxNodesValve stops the walk once the visited-node budget is exhausted.
func TestMaxNodesValve(t *testing.T) {
	// A long helper chain longer than the valve; the test entrypoint sits past
	// the budget, so it is (safely) not reached — the valve trades recall for a
	// bound, the documented behaviour.
	r := &fakeReader{inbound: map[string][]ports.NodeRef{
		"prod": {fn("h1", "helper1", "x_test.go")},
		"h1":   {fn("h2", "helper2", "x_test.go")},
		"h2":   {fn("t", "TestDeep", "x_test.go")},
	}}
	// maxNodes=2 admits the seed + h1, then stops.
	got, err := mustMap(t, r, coverage.WithMaxNodes(2)).TestsCovering(context.Background(), "repo", "main", "prod")
	if err != nil {
		t.Fatalf("TestsCovering: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (entrypoint past the node budget)", names(got))
	}
}

// TestNilReaderRejected verifies the constructor guards a nil dependency.
func TestNilReaderRejected(t *testing.T) {
	_, err := coverage.NewReverseMap(nil)
	if !errors.Is(err, coverage.ErrMissingDependency) {
		t.Fatalf("got %v, want ErrMissingDependency", err)
	}
}

// TestGoTestNameRule pins the go-test naming rule, including the Testify trap.
func TestGoTestNameRule(t *testing.T) {
	cases := map[string]bool{
		"TestFoo":      true,
		"Test":         true,
		"Test_lower":   true, // underscore is not a lowercase letter
		"BenchmarkX":   true,
		"FuzzX":        true,
		"ExampleX":     true,
		"Testify":      false, // helper: 'i' is lowercase
		"benchmarkX":   false,
		"NotATest":     false,
		"setupFixture": false,
	}
	for name, want := range cases {
		r := &fakeReader{inbound: map[string][]ports.NodeRef{
			"prod": {fn("c", name, "x_test.go")},
		}}
		got, err := mustMap(t, r).TestsCovering(context.Background(), "repo", "main", "prod")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		isEntry := len(got) == 1
		if isEntry != want {
			t.Errorf("name %q: classified entrypoint=%v, want %v", name, isEntry, want)
		}
	}
}
