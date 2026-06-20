// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"errors"
	"testing"
)

// This file LOCKS the untested-symbol gate's CALLS-edge proxy behavior on Go
// idioms that exercise a prod symbol WITHOUT a direct static CALLS edge from a
// test file (probed in ). Each test modifies a prod symbol that IS
// exercised by a test via the named idiom and asserts the CURRENT gate verdict,
// so the documented proxy limit is a regression lock, not a silent surprise.
// Outcome of the probe (see untested.go "Proxy limits"):
//   func-value (local-var, callback-arg) and table-driven struct field:
//     no edge is produced → false-FAIL. Flip the assertions here to PASS
//     once support for func-value proxy dispatch is implemented.
//   embedded method promotion WITHOUT an interface: false-FAIL.
//   embedded method satisfying an INTERFACE: already suppressed by the
//     interface-dispatch fix → PASSES today. Locked as PASS.
//   transitive-only coverage: false-FAIL. The principled fix is to use
//     a transitive reverse map.
//   a DIRECT test call PASSES (control).

// proxyProbe is one untested-gate proxy-limit fixture: a prod file (base→cand
// body change) plus the test file that exercises the prod symbol via the idiom
// under probe.
type proxyProbe struct {
	prodFile, baseProd, candProd, testFile, testSrc string
}

// probeUntestedModify seeds base with the probe's prod+test files, then modifies
// the prod file (body change) and runs the untested gate, returning whether it
// PASSED (symbol counted as tested) and the verdict for inspection.
func probeUntestedModify(t *testing.T, p proxyProbe) (untestedVerdict, error) {
	t.Helper()
	home := t.TempDir()
	dbPath := home + "/veska.db"
	seedBaseDB(t, dbPath, map[string]string{p.prodFile: p.baseProd, p.testFile: p.testSrc})
	repoDir := t.TempDir()
	c := p.candProd
	makeRepo(t, repoDir,
		map[string]string{p.prodFile: p.baseProd, p.testFile: p.testSrc},
		map[string]*string{p.prodFile: &c}, // testFile untouched
	)
	return runUntested(t, home, repoDir)
}

// assertProxyLimit asserts the gate currently FAILs (the documented false-FAIL),
// naming the flip-when follow-up so the lock doubles as a live spec.
func assertProxyLimit(t *testing.T, v untestedVerdict, err error, flipWhen string) {
	t.Helper()
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("documented proxy limit no longer FAILs - FLIP this assert to PASS and close %s; got err=%v verdict=%+v", flipWhen, err, v)
	}
}

// func-value assigned to a local var then called via the var (no CALLS edge).
func TestUntestedProxyLimit_FuncValueLocalVar(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "greet.go",
		baseProd: "package p\n\nfunc Greet() string { return \"hi\" }\n",
		candProd: "package p\n\nfunc Greet() string { return \"hello\" }\n",
		testFile: "greet_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tfn := Greet\n\tif fn() != \"hi\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	assertProxyLimit(t, v, err, "")
}

// func-value passed directly as a callback argument (cross-file → no edge).
func TestUntestedProxyLimit_FuncValueCallbackArg(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "cleanup.go",
		baseProd: "package p\n\nfunc Cleanup() {}\n",
		candProd: "package p\n\nfunc Cleanup() { _ = 1 }\n",
		testFile: "cleanup_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestCleanup(t *testing.T) {\n\tt.Cleanup(Cleanup)\n}\n",
	})
	assertProxyLimit(t, v, err, "")
}

// func-value dispatched through a table-driven struct field (no edge).
func TestUntestedProxyLimit_TableDrivenStructField(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "greet.go",
		baseProd: "package p\n\nfunc Greet() string { return \"hi\" }\n",
		candProd: "package p\n\nfunc Greet() string { return \"hello\" }\n",
		testFile: "greet_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tcases := []struct{ fn func() string }{{Greet}}\n\tfor _, c := range cases {\n\t\tif c.fn() != \"hi\" {\n\t\t\tt.Fatal(\"bad\")\n\t\t}\n\t}\n}\n",
	})
	assertProxyLimit(t, v, err, "")
}

// embedded-struct method promotion WITHOUT an interface (w.Do binds to a
// non-existent Wrap.Do, not Base.Do).
func TestUntestedProxyLimit_EmbeddedMethodNoInterface(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "base.go",
		baseProd: "package p\n\ntype Base struct{}\n\nfunc (Base) Do() string { return \"x\" }\n\ntype Wrap struct{ Base }\n",
		candProd: "package p\n\ntype Base struct{}\n\nfunc (Base) Do() string { return \"y\" }\n\ntype Wrap struct{ Base }\n",
		testFile: "base_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestDo(t *testing.T) {\n\tw := Wrap{}\n\tif w.Do() != \"x\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	assertProxyLimit(t, v, err, "")
}

// transitive-only coverage: the test calls Outer, Outer calls Inner; Inner has
// no DIRECT test caller. Principled fix is the transitive reverse map.
func TestUntestedProxyLimit_TransitiveOnly(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "calc.go",
		baseProd: "package p\n\nfunc Outer() string { return Inner() }\nfunc Inner() string { return \"x\" }\n",
		candProd: "package p\n\nfunc Outer() string { return Inner() }\nfunc Inner() string { return \"y\" }\n",
		testFile: "calc_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestOuter(t *testing.T) {\n\tif Outer() != \"x\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	assertProxyLimit(t, v, err, "")
}

// reflection / generated harness: a method invoked by string name via reflect
// (MethodByName) can NEVER produce a static edge - this is a PERMANENT proxy
// limit, asserted FAIL with no flip-when target (unlike the locks above).
func TestUntestedProxyLimit_ReflectionDispatch_Permanent(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "svc.go",
		baseProd: "package p\n\ntype S struct{}\n\nfunc (S) Do() string { return \"x\" }\n",
		candProd: "package p\n\ntype S struct{}\n\nfunc (S) Do() string { return \"y\" }\n",
		testFile: "svc_test.go",
		testSrc:  "package p\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n)\n\nfunc TestDo(t *testing.T) {\n\tm := reflect.ValueOf(S{}).MethodByName(\"Do\")\n\tif m.Call(nil)[0].String() != \"x\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("reflection dispatch is a PERMANENT proxy limit and must FAIL (no static edge possible); got err=%v verdict=%+v", err, v)
	}
}

// The actual embedded case - base method satisfying an interface via
// embedding - is already suppressed by the interface-dispatch fix (zvh6.9), so
// it must PASS. This locks that coverage so a regression there surfaces here.
func TestUntested_EmbeddedInterfaceMethod_Passes(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "base.go",
		baseProd: "package p\n\ntype Doer interface{ Do() string }\n\ntype Base struct{}\n\nfunc (Base) Do() string { return \"x\" }\n\ntype Wrap struct{ Base }\n",
		candProd: "package p\n\ntype Doer interface{ Do() string }\n\ntype Base struct{}\n\nfunc (Base) Do() string { return \"y\" }\n\ntype Wrap struct{ Base }\n",
		testFile: "base_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestDo(t *testing.T) {\n\tvar d Doer = Wrap{}\n\tif d.Do() != \"x\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	if err != nil || !v.Pass {
		t.Fatalf("embedded method satisfying an interface must PASS (suppressed by zvh6.9); got err=%v verdict=%+v", err, v)
	}
}

// Control: a DIRECT test call must PASS - proves the harness wiring.
func TestUntested_DirectCall_Passes(t *testing.T) {
	v, err := probeUntestedModify(t, proxyProbe{
		prodFile: "greet.go",
		baseProd: "package p\n\nfunc Greet() string { return \"hi\" }\n",
		candProd: "package p\n\nfunc Greet() string { return \"hello\" }\n",
		testFile: "greet_test.go",
		testSrc:  "package p\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet() != \"hi\" {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	if err != nil || !v.Pass {
		t.Fatalf("direct test call must PASS (control); got err=%v verdict=%+v", err, v)
	}
}
