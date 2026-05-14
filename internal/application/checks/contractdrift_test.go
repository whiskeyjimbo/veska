package checks_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeDriftQuerier returns a preconfigured list of drifted nodes regardless of
// inputs (filtered by the requested file paths so empty-paths semantics work)
// and remembers call args for assertions.
type fakeDriftQuerier struct {
	drifted   []ports.DriftedNode
	err       error
	gotRepo   string
	gotBranch string
	gotPaths  []string
	callCount int
}

func (f *fakeDriftQuerier) DriftedNodesInFiles(_ context.Context, repoID, branch string, filePaths []string) ([]ports.DriftedNode, error) {
	f.callCount++
	f.gotRepo = repoID
	f.gotBranch = branch
	f.gotPaths = filePaths
	if f.err != nil {
		return nil, f.err
	}
	if len(filePaths) == 0 {
		return nil, nil
	}
	allow := make(map[string]struct{}, len(filePaths))
	for _, p := range filePaths {
		allow[p] = struct{}{}
	}
	var out []ports.DriftedNode
	for _, n := range f.drifted {
		if _, ok := allow[n.FilePath]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func TestContractDriftCheck_Name(t *testing.T) {
	c := checks.NewContractDriftCheck(&fakeDriftQuerier{})
	if c.Name() != "contract-drift" {
		t.Errorf("Name() = %q, want %q", c.Name(), "contract-drift")
	}
}

func TestContractDriftCheck_ImplementsCheck(t *testing.T) {
	var _ checks.Check = checks.NewContractDriftCheck(&fakeDriftQuerier{})
}

func TestContractDriftCheck_EmitsFindingShape(t *testing.T) {
	q := &fakeDriftQuerier{
		drifted: []ports.DriftedNode{
			{
				NodeID:    "n-foo",
				FilePath:  "pkg/a.go",
				Kind:      "function",
				Name:      "Foo",
				PrevSig:   "func Foo() error",
				NewSig:    "func Foo(ctx context.Context) error",
				LineStart: 10, LineEnd: 20,
			},
		},
	}
	c := checks.NewContractDriftCheck(q)

	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "repo1", Branch: "main", GitSHA: "abc",
		FilePaths: []string{"pkg/a.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings: got %d, want 1", len(findings))
	}
	f := findings[0]

	if f.Rule != "contract-drift" {
		t.Errorf("rule = %q, want contract-drift", f.Rule)
	}
	if f.SourceLayer != domain.LayerStructural {
		t.Errorf("source_layer = %q, want structural", f.SourceLayer)
	}
	if f.Severity != domain.SeverityMedium {
		t.Errorf("severity = %q, want medium", f.Severity)
	}
	if f.RepoID != "repo1" || f.Branch != "main" {
		t.Errorf("repo/branch propagation wrong: %q/%q", f.RepoID, f.Branch)
	}
	if f.NodeID == nil || *f.NodeID != "n-foo" {
		t.Errorf("anchor: want node_id=n-foo, got %v", f.NodeID)
	}
	if f.FilePath != nil {
		t.Errorf("anchor: file_path should be nil for node anchor, got %v", f.FilePath)
	}
	if f.FindingID == "" {
		t.Errorf("FindingID empty")
	}
	if !strings.Contains(f.Message, "func Foo() error") || !strings.Contains(f.Message, "func Foo(ctx context.Context) error") {
		t.Errorf("message missing prev/new sig snippet: %q", f.Message)
	}
}

// TestContractDriftCheck_FindingIDStableAcrossRuns verifies idempotency: the
// branch-stable finding_id depends only on (rule, node_id) so re-running the
// same input produces the same id.
func TestContractDriftCheck_FindingIDStableAcrossRuns(t *testing.T) {
	q := &fakeDriftQuerier{drifted: []ports.DriftedNode{
		{NodeID: "n-foo", FilePath: "a.go", Kind: "function", Name: "Foo", PrevSig: "a", NewSig: "b"},
	}}
	c := checks.NewContractDriftCheck(q)

	in := checks.Input{RepoID: "r", Branch: "main", FilePaths: []string{"a.go"}}

	r1, _ := c.Run(context.Background(), in)
	r2, _ := c.Run(context.Background(), in)
	if len(r1) != 1 || len(r2) != 1 {
		t.Fatalf("expected one finding per run, got %d/%d", len(r1), len(r2))
	}
	if r1[0].FindingID != r2[0].FindingID {
		t.Errorf("finding_id not stable: %q vs %q", r1[0].FindingID, r2[0].FindingID)
	}
}

func TestContractDriftCheck_EmptyFilePathsIsNoOp(t *testing.T) {
	q := &fakeDriftQuerier{drifted: []ports.DriftedNode{
		{NodeID: "x", FilePath: "a.go", Kind: "function", Name: "X", PrevSig: "a", NewSig: "b"},
	}}
	c := checks.NewContractDriftCheck(q)

	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: nil,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected zero findings for empty FilePaths, got %d", len(findings))
	}
	if q.callCount != 0 {
		t.Errorf("querier should not be consulted for empty FilePaths, got %d calls", q.callCount)
	}
}

func TestContractDriftCheck_NoDriftEmitsNoFinding(t *testing.T) {
	q := &fakeDriftQuerier{drifted: nil}
	c := checks.NewContractDriftCheck(q)

	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"a.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("no drift -> no findings; got %d", len(findings))
	}
}

func TestContractDriftCheck_QuerierErrorPropagates(t *testing.T) {
	q := &fakeDriftQuerier{err: errors.New("boom")}
	c := checks.NewContractDriftCheck(q)

	_, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"a.go"},
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "contract-drift") {
		t.Errorf("error not annotated with check name: %v", err)
	}
}

func TestContractDriftCheck_NilQuerierReturnsError(t *testing.T) {
	c := checks.NewContractDriftCheck(nil)
	_, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"a.go"},
	})
	if err == nil {
		t.Fatalf("expected error from nil querier")
	}
}

// TestContractDriftCheck_PassesScopeThrough verifies the check forwards
// (repoID, branch, filePaths) verbatim to the querier — the storage layer is
// responsible for scoping the search.
func TestContractDriftCheck_PassesScopeThrough(t *testing.T) {
	q := &fakeDriftQuerier{}
	c := checks.NewContractDriftCheck(q)

	_, _ = c.Run(context.Background(), checks.Input{
		RepoID: "repoZ", Branch: "feat/x", FilePaths: []string{"x.go", "y.go"},
	})
	if q.gotRepo != "repoZ" || q.gotBranch != "feat/x" {
		t.Errorf("scope: got repo=%q branch=%q", q.gotRepo, q.gotBranch)
	}
	if len(q.gotPaths) != 2 || q.gotPaths[0] != "x.go" || q.gotPaths[1] != "y.go" {
		t.Errorf("paths: got %v", q.gotPaths)
	}
}
