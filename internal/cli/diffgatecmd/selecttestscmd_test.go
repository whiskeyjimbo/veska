package diffgatecmd

import (
	"reflect"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/coverage"
)

func tref(name, file string) coverage.TestRef {
	return coverage.TestRef{Name: name, FilePath: file}
}

// TestBuildSelectionReport_GroupsAndAnchors asserts covering tests are grouped
// per package and rendered as an anchored `^(.)$` -run command.
func TestBuildSelectionReport_GroupsAndAnchors(t *testing.T) {
	tests := []coverage.TestRef{
		tref("TestB", "pkg/a/a_test.go"),
		tref("TestA", "pkg/a/a_test.go"),
		tref("TestC", "pkg/b/b_test.go"),
	}
	rep := buildSelectionReport(tests, nil)

	if rep.Empty {
		t.Fatalf("Empty=true, want false")
	}
	if len(rep.Packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(rep.Packages))
	}
	// Packages sorted; tests sorted within a package.
	a := rep.Packages[0]
	if a.Package != "pkg/a" || !reflect.DeepEqual(a.Tests, []string{"TestA", "TestB"}) {
		t.Errorf("pkg/a = %+v, want sorted TestA,TestB", a)
	}
	if a.Command != "go test -run '^(TestA|TestB)$' ./pkg/a" {
		t.Errorf("pkg/a command = %q", a.Command)
	}
	if a.RunAll {
		t.Errorf("pkg/a RunAll=true, want false")
	}
	b := rep.Packages[1]
	if b.Command != "go test -run '^(TestC)$' ./pkg/b" {
		t.Errorf("pkg/b command = %q", b.Command)
	}
}

// TestBuildSelectionReport_ForcedPackageRunsAll: a changed test file forces its
// whole package (no -run filter), even when specific tests were also selected
// for it from a prod change.
func TestBuildSelectionReport_ForcedPackageRunsAll(t *testing.T) {
	tests := []coverage.TestRef{tref("TestA", "pkg/a/a_test.go")}
	forced := map[string]struct{}{"pkg/a": {}}
	rep := buildSelectionReport(tests, forced)

	if len(rep.Packages) != 1 {
		t.Fatalf("got %d packages, want 1", len(rep.Packages))
	}
	p := rep.Packages[0]
	if !p.RunAll {
		t.Errorf("RunAll=false, want true (changed test file forces the package)")
	}
	if len(p.Tests) != 0 {
		t.Errorf("Tests=%v, want empty when RunAll", p.Tests)
	}
	if p.Command != "go test ./pkg/a" {
		t.Errorf("command = %q, want unfiltered go test ./pkg/a", p.Command)
	}
}

// TestBuildSelectionReport_ForcedPackageNoCoveringTests: a changed test file in
// a package with no prod-derived selection still appears (run-all).
func TestBuildSelectionReport_ForcedPackageOnly(t *testing.T) {
	forced := map[string]struct{}{"pkg/x": {}}
	rep := buildSelectionReport(nil, forced)

	if rep.Empty {
		t.Fatalf("Empty=true, want false (a package was forced)")
	}
	if len(rep.Packages) != 1 || !rep.Packages[0].RunAll || rep.Packages[0].Package != "pkg/x" {
		t.Fatalf("got %+v, want single run-all pkg/x", rep.Packages)
	}
}

// TestBuildSelectionReport_Empty: no covering tests and no changed test files
// yields an explicit empty selection, never "all tests" (AC2).
func TestBuildSelectionReport_Empty(t *testing.T) {
	rep := buildSelectionReport(nil, nil)
	if !rep.Empty {
		t.Fatalf("Empty=false, want true")
	}
	if len(rep.Packages) != 0 || len(rep.Commands) != 0 {
		t.Errorf("packages/commands non-empty on empty selection: %+v", rep)
	}
	if rep.Note == "" {
		t.Errorf("Note empty; want an explicit no-tests explanation")
	}
}

// TestGoTestCommand pins the runner-format rule for each mode.
func TestGoTestCommand(t *testing.T) {
	cases := []struct {
		pkg    string
		runAll bool
		tests  []string
		want   string
	}{
		{"internal/foo", false, []string{"TestA", "TestB"}, "go test -run '^(TestA|TestB)$' ./internal/foo"},
		{"internal/foo", true, nil, "go test ./internal/foo"},
		{"internal/foo", false, nil, "go test ./internal/foo"}, // defensive: no tests => run-all form
		{"internal/foo", false, []string{"TestSolo"}, "go test -run '^(TestSolo)$' ./internal/foo"},
	}
	for _, c := range cases {
		got := goTestCommand(c.pkg, c.runAll, c.tests)
		if got != c.want {
			t.Errorf("goTestCommand(%q,%v,%v) = %q, want %q", c.pkg, c.runAll, c.tests, got, c.want)
		}
	}
}
