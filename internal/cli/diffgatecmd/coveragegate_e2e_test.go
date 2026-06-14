package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
)

// Base fixture: Foo is a prod function covered by a test in a SEPARATE file.
const (
	fooSrc     = "package p\n\nfunc Foo() int { return 1 }\n"
	fooTestSrc = "package p\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) { _ = Foo() }\n"
)

func runUntested(t *testing.T, home, repoDir string) (untestedVerdict, error) {
	t.Helper()
	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunUntested(context.Background(), UntestedParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	var v untestedVerdict
	if jerr := json.Unmarshal(out.Bytes(), &v); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	return v, err
}

type untestedVerdict struct {
	Pass            bool     `json:"pass"`
	Failures        []string `json:"failures"`
	UntestedChanged []struct {
		NodeID  string `json:"node_id"`
		Message string `json:"message"`
	} `json:"untested_changed"`
}

// False-FAIL lock (catastrophic if wrong): modifying a prod symbol whose test
// lives in an UNCHANGED file must PASS. Verifies the re-promote of the changed
// prod file PRESERVES the base test→prod CALLS edge (node_id stable; inbound
// edge survives). If it didn't, every modified-but-tested function would
// false-FAIL and the gate would be dead on arrival.
func TestRunUntested_E2E_ModifiedButTested_Passes(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	modified := "package p\n\nfunc Foo() int { return 2 }\n" // body change, same signature/test
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"foo.go": &modified}, // foo_test.go untouched
	)

	v, err := runUntested(t, home, repoDir)
	if err != nil {
		t.Fatalf("modified-but-tested must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("modified-but-tested must PASS; got %+v", v)
	}
}

// Persona-test regression: modifying a concrete method tested ONLY via
// interface dispatch must PASS. The static CALLS edge points at the interface
// method, not the impl, so without interface-method suppression the gate
// false-FAILs a ubiquitous Go idiom (the filed P1).
func TestRunUntested_E2E_InterfaceDispatchTested_Passes(t *testing.T) {
	home := t.TempDir()
	const ifaceSrc = "package p\n\ntype Greeter interface{ Greet() string }\n\ntype EN struct{}\n\nfunc (EN) Greet() string { return \"hi\" }\n\nfunc Pick(g Greeter) string { return g.Greet() }\n"
	const ifaceTestSrc = "package p\n\nimport \"testing\"\n\nfunc TestPick(t *testing.T) {\n\tif Pick(EN{}) != \"hi\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"greeter.go": ifaceSrc, "greeter_test.go": ifaceTestSrc,
	})
	repoDir := t.TempDir()
	// Modify EN.Greet's body (it stays an interface-dispatch-tested method).
	modified := "package p\n\ntype Greeter interface{ Greet() string }\n\ntype EN struct{}\n\nfunc (EN) Greet() string { return \"hi!\" }\n\nfunc Pick(g Greeter) string { return g.Greet() }\n"
	makeRepo(t, repoDir,
		map[string]string{"greeter.go": ifaceSrc, "greeter_test.go": ifaceTestSrc},
		map[string]*string{"greeter.go": &modified},
	)

	v, err := runUntested(t, home, repoDir)
	if err != nil {
		t.Fatalf("interface-dispatch-tested method must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("interface-dispatch-tested method must PASS (P1 regression); got %+v", v)
	}
}

// Discriminating case (advisor): interface declared in an UNCHANGED file,
// impl in a CHANGED file. Verifies the interface-method lister sees the
// interface even though only the impl file was re-promoted — the clone is a
// full base snapshot, so unchanged-file interface nodes survive (unlike the
// cascade-deleted caller edges). Must PASS.
func TestRunUntested_E2E_InterfaceInUnchangedFile_Passes(t *testing.T) {
	home := t.TempDir()
	const ifaceFile = "package p\n\ntype Greeter interface{ Greet() string }\n\nfunc Pick(g Greeter) string { return g.Greet() }\n"
	const implFile = "package p\n\ntype EN struct{}\n\nfunc (EN) Greet() string { return \"hi\" }\n"
	const testFile = "package p\n\nimport \"testing\"\n\nfunc TestPick(t *testing.T) {\n\tif Pick(EN{}) != \"hi\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"iface.go": ifaceFile, "impl.go": implFile, "greeter_test.go": testFile,
	})
	repoDir := t.TempDir()
	modImpl := "package p\n\ntype EN struct{}\n\nfunc (EN) Greet() string { return \"hi!\" }\n" // modify impl only
	makeRepo(t, repoDir,
		map[string]string{"iface.go": ifaceFile, "impl.go": implFile, "greeter_test.go": testFile},
		map[string]*string{"impl.go": &modImpl}, // iface.go unchanged
	)

	v, err := runUntested(t, home, repoDir)
	if err != nil {
		t.Fatalf("interface-in-unchanged-file must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("interface-in-unchanged-file must PASS; got %+v", v)
	}
}

// solov2-d521 regression (gate level): a prod METHOD tested only via a
// method-call dispatch `recv.Method()` in a SEPARATE test file must PASS when
// its body changes. Before the parser resolved value-receiver method calls the
// test→method CALLS edge never existed, so the method false-FAILed as untested
// — the junior-persona P1. Cross-file (method in order.go, test in
// order_test.go) so it also exercises promotion-time bare-name binding of the
// rewritten "Order.Total" callee.
func TestRunUntested_E2E_MethodCallTested_Passes(t *testing.T) {
	home := t.TempDir()
	const prodSrc = "package p\n\ntype Order struct{ Qty int }\n\nfunc (o Order) Total() int { return o.Qty }\n"
	const testSrc = "package p\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\to := Order{Qty: 2}\n\tif o.Total() != 2 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"order.go": prodSrc, "order_test.go": testSrc,
	})
	repoDir := t.TempDir()
	modified := "package p\n\ntype Order struct{ Qty int }\n\nfunc (o Order) Total() int { return o.Qty * 1 }\n" // body change
	makeRepo(t, repoDir,
		map[string]string{"order.go": prodSrc, "order_test.go": testSrc},
		map[string]*string{"order.go": &modified}, // order_test.go untouched
	)

	v, err := runUntested(t, home, repoDir)
	if err != nil {
		t.Fatalf("method tested via recv.Method() must PASS (d521); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("method-call-tested method must PASS (d521 regression); got %+v", v)
	}
}

// False-PASS lock (dangerous direction): adding a prod symbol with no test must
// FAIL, and the gate must list it. Proves the cross-machinery join fires —
// Ephemeral.ChangedNodeIDs (overlay-derived) and the untested finding's NodeID
// (re-promote-clone-derived) agree on the node_id. A silent disagreement would
// empty the intersection -> the gate never fires -> false PASS. Non-substitutable
// by fakes: real parse + real promote on both sides.
func TestRunUntested_E2E_AddedUntested_Fails(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	barSrc := "package p\n\nfunc Bar() int { return 9 }\n" // new prod symbol, NO test
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"bar.go": &barSrc},
	)

	v, err := runUntested(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("added-untested must FAIL (ErrGateFailed); got %v verdict=%+v", err, v)
	}
	if v.Pass || len(v.UntestedChanged) != 1 {
		t.Fatalf("want exactly one untested changed symbol (Bar); got %+v", v)
	}
	if len(v.Failures) != 1 || v.Failures[0] != diffgate.FailUntestedChanged {
		t.Fatalf("failures = %v", v.Failures)
	}
}

// Test-removal lock (the union's dangerous direction): modifying a prod symbol
// AND deleting its test must FAIL. base still lists the now-gone test as a
// caller, but it lives in a CHANGED file, so the clone (where the test is gone)
// is authoritative — the union must drop the stale base caller. This also
// self-validates that CallerFiles and ChangedFiles share a path format: a
// format mismatch makes the filter a no-op and this test goes green (PASS).
func TestRunUntested_E2E_ModifyProdRemoveTest_Fails(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	modified := "package p\n\nfunc Foo() int { return 2 }\n"
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"foo.go": &modified, "foo_test.go": nil}, // modify Foo, delete its test
	)

	v, err := runUntested(t, home, repoDir)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("modify-prod + delete-test must FAIL (ErrGateFailed); got %v verdict=%+v", err, v)
	}
	if v.Pass || len(v.UntestedChanged) != 1 {
		t.Fatalf("want one untested changed symbol (Foo, test removed); got %+v", v)
	}
}

// AC2 positive — the case that justifies the re-promote: adding a prod symbol
// AND its test in a new _test.go (cross-file) must PASS. The test→prod CALLS
// edge resolves at promotion (the ephemeral overlay alone would miss it).
func TestRunUntested_E2E_AddedWithTest_Passes(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	barSrc := "package p\n\nfunc Bar() int { return 9 }\n"
	barTestSrc := "package p\n\nimport \"testing\"\n\nfunc TestBar(t *testing.T) { _ = Bar() }\n"
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"bar.go": &barSrc, "bar_test.go": &barTestSrc},
	)

	v, err := runUntested(t, home, repoDir)
	if err != nil {
		t.Fatalf("added-with-test must PASS (nil err); got %v verdict=%+v", err, v)
	}
	if !v.Pass {
		t.Fatalf("added-with-test (cross-file) must PASS; got %+v", v)
	}
}
