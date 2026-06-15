package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func runSelectTests(t *testing.T, home, repoDir string) (selectTestsReport, error) {
	t.Helper()
	t.Setenv("VESKA_HOME", home)
	var out bytes.Buffer
	err := RunSelectTests(context.Background(), SelectTestsParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	var rep selectTestsReport
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("selection JSON: %v\nraw: %s", jerr, out.String())
	}
	return rep, err
}

// TestRunSelectTests_E2E_SelectsCoveringTest is the positive integration proof:
// over a REAL cold-scanned graph, modifying prod Foo selects the test that
// transitively calls it. This is also the only check that verifies the
// isTestEntrypoint assumption against a real parse — that a Go test function
// node lands as kind="function" with symbol_path="TestFoo". If tree-sitter
// tagged test funcs differently, the selection would be empty and this fails.
func TestRunSelectTests_E2E_SelectsCoveringTest(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	modified := "package p\n\nfunc Foo() int { return 2 }\n" // body change, same test covers it
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"foo.go": &modified}, // only foo.go changes; foo_test.go untouched
	)

	rep, err := runSelectTests(t, home, repoDir)
	if err != nil {
		t.Fatalf("RunSelectTests: %v", err)
	}
	if rep.Empty {
		t.Fatalf("selection empty; TestFoo transitively calls the changed Foo. report: %+v", rep)
	}

	var selected *packageSelection
	for i := range rep.Packages {
		for _, tn := range rep.Packages[i].Tests {
			if tn == "TestFoo" {
				selected = &rep.Packages[i]
			}
		}
	}
	if selected == nil {
		t.Fatalf("TestFoo not selected for changed Foo; got packages %+v", rep.Packages)
	}
	// foo_test.go is unchanged, so the package is run via -run filter (not run-all).
	if selected.RunAll {
		t.Errorf("package RunAll=true; foo_test.go was unchanged, want a -run filter")
	}
	if !strings.Contains(selected.Command, "-run '^(") || !strings.Contains(selected.Command, "TestFoo") {
		t.Errorf("command not an anchored -run selection: %q", selected.Command)
	}
}

// TestRunSelectTests_E2E_UnknownRepo_AdvisoryEmpty: an unknown --repo handle is
// reported as an empty selection + a distinct "unknown repo" error at exit 0
// (nil err) — the never-gates contract (v6de.3) and the unknown-vs-unindexed
// distinction (i0tx.2 F2).
func TestRunSelectTests_E2E_UnknownRepo_AdvisoryEmpty(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{"foo.go": fooSrc})
	t.Setenv("VESKA_HOME", home)

	var out bytes.Buffer
	err := RunSelectTests(context.Background(), SelectTestsParams{
		RepoID: "no-such-repo", Branch: discBranch, RepoRoot: t.TempDir(),
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	if err != nil {
		t.Fatalf("unknown repo must be advisory (nil err); got %v", err)
	}
	var rep selectTestsReport
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("must still emit JSON: %v\nraw: %s", jerr, out.String())
	}
	if !rep.Empty || !strings.Contains(rep.Error, "unknown repo") {
		t.Fatalf("want empty + 'unknown repo' error; got %+v", rep)
	}
}

// TestRunSelectTests_E2E_BadRef_AdvisoryEmpty: a bad base ref yields a clean
// ref-naming error (not raw git plumbing) as an advisory empty selection at
// exit 0 (v6de.3 + i0tx.2 F3).
func TestRunSelectTests_E2E_BadRef_AdvisoryEmpty(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	mod := "package p\n\nfunc Foo() int { return 2 }\n"
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"foo.go": &mod},
	)
	t.Setenv("VESKA_HOME", home)

	var out bytes.Buffer
	err := RunSelectTests(context.Background(), SelectTestsParams{
		RepoID: discRepo, Branch: discBranch, RepoRoot: repoDir,
		BaseRef: "no-such-ref", CandidateRef: "HEAD", Out: &out,
	})
	if err != nil {
		t.Fatalf("bad ref must be advisory (nil err); got %v", err)
	}
	var rep selectTestsReport
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("must still emit JSON: %v\nraw: %s", jerr, out.String())
	}
	if !rep.Empty || !strings.Contains(rep.Error, "ref not found") {
		t.Fatalf("want empty + clean 'ref not found' error; got %+v\nraw: %s", rep, out.String())
	}
}

// TestRunSelectTests_E2E_ChangedTestFileForcesPackage: editing a *_test.go file
// forces its whole package (run-all), since a newly-added/edited test may not be
// in the index — the safe over-selecting direction.
func TestRunSelectTests_E2E_ChangedTestFileForcesPackage(t *testing.T) {
	home := t.TempDir()
	seedBaseDB(t, filepath.Join(home, "veska.db"), map[string]string{
		"foo.go": fooSrc, "foo_test.go": fooTestSrc,
	})
	repoDir := t.TempDir()
	// Change ONLY the test file (add a second test); foo.go untouched.
	modifiedTest := fooTestSrc + "\nfunc TestFooAgain(t *testing.T) { _ = Foo() }\n"
	makeRepo(t, repoDir,
		map[string]string{"foo.go": fooSrc, "foo_test.go": fooTestSrc},
		map[string]*string{"foo_test.go": &modifiedTest},
	)

	rep, err := runSelectTests(t, home, repoDir)
	if err != nil {
		t.Fatalf("RunSelectTests: %v", err)
	}
	if rep.Empty || len(rep.Packages) != 1 {
		t.Fatalf("want one forced package, got %+v", rep)
	}
	p := rep.Packages[0]
	if !p.RunAll {
		t.Errorf("changed test file must force run-all; got %+v", p)
	}
	if strings.Contains(p.Command, "-run") {
		t.Errorf("forced package must run all (no -run): %q", p.Command)
	}
}
