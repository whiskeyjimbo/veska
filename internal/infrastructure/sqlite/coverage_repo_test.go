package sqlite_test

import (
	"context"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestCoverageRepo_AttributesCallersByFile seeds a prod symbol called from both
// a prod and a test file, a prod symbol called only from a prod file, and an
// uncalled symbol; it asserts the adapter returns each candidate paired with
// the distinct file paths of its direct CALLS callers.
func TestCoverageRepo_AttributesCallersByFile(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t) // reuse repos+nodes+edges fixture helpers

	// Candidates (in scope: pkg/a.go).
	f.insertNode(t, "n-prod", "pkg/a.go", "function", "DoWork")
	f.insertNode(t, "n-untested", "pkg/a.go", "function", "lonely")
	f.insertNode(t, "n-orphan", "pkg/a.go", "function", "neverCalled")
	// Callers (out of scope; their file paths are what matters).
	f.insertNode(t, "c-prod", "pkg/handler.go", "function", "handle")
	f.insertNode(t, "c-test", "pkg/a_test.go", "function", "TestDoWork")

	// CALLS edges. n-prod gets a prod AND a test caller; n-untested only prod.
	f.insertEdge(t, "e1", "c-prod", "n-prod", "calls")
	f.insertEdge(t, "e2", "c-test", "n-prod", "calls")
	f.insertEdge(t, "e3", "c-prod", "n-untested", "calls")
	// A non-CALLS edge must NOT count as a caller.
	f.insertEdge(t, "e4", "c-test", "n-untested", "contains")

	repo := sqlite.NewCoverageRepo(f.db)
	got, err := repo.CandidateCallersInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("CandidateCallersInFiles: %v", err)
	}

	byID := make(map[string][]string, len(got))
	for _, nc := range got {
		files := append([]string(nil), nc.CallerFiles...)
		sort.Strings(files)
		byID[nc.Node.NodeID] = files
	}

	// All three candidates returned.
	for _, id := range []string{"n-prod", "n-untested", "n-orphan"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("candidate %s missing from result", id)
		}
	}
	if want := []string{"pkg/a_test.go", "pkg/handler.go"}; !equalStrings(byID["n-prod"], want) {
		t.Errorf("n-prod callers = %v, want %v", byID["n-prod"], want)
	}
	if want := []string{"pkg/handler.go"}; !equalStrings(byID["n-untested"], want) {
		t.Errorf("n-untested callers = %v, want %v (contains edge must not count)", byID["n-untested"], want)
	}
	if len(byID["n-orphan"]) != 0 {
		t.Errorf("n-orphan callers = %v, want empty", byID["n-orphan"])
	}
}

func TestCoverageRepo_EmptyFilePathsNoOp(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)
	repo := sqlite.NewCoverageRepo(f.db)
	got, err := repo.CandidateCallersInFiles(context.Background(), f.repoID, f.branch, nil)
	if err != nil {
		t.Fatalf("CandidateCallersInFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty filePaths returned %d rows, want 0", len(got))
	}
}

// Carry the NodeRef shape through so the check can anchor a finding.
func TestCoverageRepo_PopulatesNodeRef(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)
	f.insertNode(t, "n1", "pkg/a.go", "function", "Foo")
	repo := sqlite.NewCoverageRepo(f.db)
	got, err := repo.CandidateCallersInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("CandidateCallersInFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	n := got[0].Node
	if n.NodeID != "n1" || n.Kind != "function" || n.Name != "Foo" || n.FilePath != "pkg/a.go" {
		t.Errorf("NodeRef = %+v, want id=n1 kind=function name=Foo file=pkg/a.go", n)
	}
	if n.ContentHash == "" {
		t.Errorf("ContentHash not populated; want h-n1 from fixture")
	}
}

// TestCoverageRepo_InboundCallsEdges seeds a prod node with a CALLS caller and a
// CONTAINS caller, plus a second queried node with no caller. It asserts the
// adapter returns CALLS callers only (metadata-hydrated) and honours the
// present-key contract for an uncalled queried id.
func TestCoverageRepo_InboundCallsEdges(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)
	f.insertNode(t, "prod", "pkg/a.go", "function", "DoWork")
	f.insertNode(t, "orphan", "pkg/a.go", "function", "neverCalled")
	f.insertNode(t, "caller", "pkg/a_test.go", "function", "TestDoWork")
	f.insertNode(t, "pkgnode", "pkg/a.go", "package", "pkg")

	f.insertEdge(t, "e1", "caller", "prod", "calls")     // a real caller
	f.insertEdge(t, "e2", "pkgnode", "prod", "contains") // must NOT count

	repo := sqlite.NewCoverageRepo(f.db)
	got, err := repo.InboundCallsEdges(context.Background(), f.repoID, f.branch, []string{"prod", "orphan"})
	if err != nil {
		t.Fatalf("InboundCallsEdges: %v", err)
	}

	// Present-key contract: both queried ids present.
	if _, ok := got["orphan"]; !ok {
		t.Errorf("orphan missing from result map (present-key contract)")
	}
	if len(got["orphan"]) != 0 {
		t.Errorf("orphan callers = %v, want empty", got["orphan"])
	}
	// prod: exactly the CALLS caller, fully hydrated; the contains edge dropped.
	callers := got["prod"]
	if len(callers) != 1 {
		t.Fatalf("prod callers = %v, want exactly 1 (contains edge must not count)", callers)
	}
	c := callers[0]
	if c.NodeID != "caller" || c.Kind != "function" || c.Name != "TestDoWork" || c.FilePath != "pkg/a_test.go" {
		t.Errorf("caller NodeRef = %+v, want id=caller kind=function name=TestDoWork file=pkg/a_test.go", c)
	}
}

func TestCoverageRepo_InboundCallsEdges_EmptyNoOp(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)
	repo := sqlite.NewCoverageRepo(f.db)
	got, err := repo.InboundCallsEdges(context.Background(), f.repoID, f.branch, nil)
	if err != nil {
		t.Fatalf("InboundCallsEdges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty nodeIDs returned %d entries, want 0", len(got))
	}
}
