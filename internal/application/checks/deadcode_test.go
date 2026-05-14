package checks_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeDeadQuerier returns a preconfigured list of "dead" nodes regardless of
// inputs, and remembers the last call args for assertions.
type fakeDeadQuerier struct {
	dead      []ports.NodeRef
	err       error
	gotRepo   string
	gotBranch string
	gotPaths  []string
	callCount int
}

func (f *fakeDeadQuerier) DeadNodesInFiles(_ context.Context, repoID, branch string, filePaths []string) ([]ports.NodeRef, error) {
	f.callCount++
	f.gotRepo = repoID
	f.gotBranch = branch
	f.gotPaths = filePaths
	if f.err != nil {
		return nil, f.err
	}
	// Filter to the requested file paths so empty-paths semantics are honoured by the fake.
	if len(filePaths) == 0 {
		return nil, nil
	}
	allow := make(map[string]struct{}, len(filePaths))
	for _, p := range filePaths {
		allow[p] = struct{}{}
	}
	var out []ports.NodeRef
	for _, n := range f.dead {
		if _, ok := allow[n.FilePath]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func TestDeadCodeCheck_Name(t *testing.T) {
	c := checks.NewDeadCodeCheck(&fakeDeadQuerier{})
	if c.Name() != "dead-code" {
		t.Errorf("Name() = %q, want %q", c.Name(), "dead-code")
	}
}

func TestDeadCodeCheck_ImplementsCheck(t *testing.T) {
	var _ checks.Check = checks.NewDeadCodeCheck(&fakeDeadQuerier{})
}

// 2. Given some dead nodes, Run returns one finding per node with the correct shape.
func TestDeadCodeCheck_EmitsFindingPerDeadNode(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{
			{NodeID: "n-helper", FilePath: "pkg/a.go", Kind: "function", Name: "helper", LineStart: 10, LineEnd: 20},
			{NodeID: "n-other", FilePath: "pkg/b.go", Kind: "function", Name: "other", LineStart: 5, LineEnd: 9},
		},
	}
	c := checks.NewDeadCodeCheck(q)

	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "repo1", Branch: "main", GitSHA: "abc",
		FilePaths: []string{"pkg/a.go", "pkg/b.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings: got %d, want 2", len(findings))
	}

	sort.Slice(findings, func(i, j int) bool { return *findings[i].NodeID < *findings[j].NodeID })

	got := findings[0]
	if got.Rule != "dead-code" {
		t.Errorf("Rule = %q, want dead-code", got.Rule)
	}
	if got.SourceLayer != domain.LayerStructural {
		t.Errorf("SourceLayer = %q, want structural", got.SourceLayer)
	}
	if got.Severity != domain.SeverityLow {
		t.Errorf("Severity = %q, want low", got.Severity)
	}
	if got.NodeID == nil || *got.NodeID != "n-helper" {
		t.Errorf("NodeID anchor = %v, want n-helper", got.NodeID)
	}
	if got.FilePath != nil {
		t.Errorf("FilePath anchor = %v, want nil (node-anchored)", got.FilePath)
	}
	if got.RepoID != "repo1" || got.Branch != "main" {
		t.Errorf("repo/branch = %q/%q", got.RepoID, got.Branch)
	}
	// Branch-stable finding_id is computed; just confirm it's populated.
	if got.FindingID == "" {
		t.Errorf("FindingID empty")
	}
}

// 5. Filter regressions: main / init / Test* / Example* / Benchmark* / uppercase-leading produce no finding.
func TestDeadCodeCheck_AppliesAllowlistFilters(t *testing.T) {
	cases := []struct {
		name       string
		nodeName   string
		kind       string
		wantFilter bool
	}{
		{"main is excluded", "main", "function", true},
		{"init is excluded", "init", "function", true},
		{"Test prefix excluded", "TestFoo", "function", true},
		{"Example prefix excluded", "ExampleFoo", "function", true},
		{"Benchmark prefix excluded", "BenchmarkFoo", "function", true},
		{"Uppercase-leading (exported) excluded", "DoThing", "function", true},
		{"lowercase function reported", "helper", "function", false},
		{"lowercase method reported", "doit", "method", false},
		// non function/method kinds still subject only to name rules — main on a kind that
		// is not function/method is still uppercase-rule-eligible (lowercase 'main' would be reported).
		{"unrelated kind: type 'Foo' still uppercase-excluded", "Foo", "type", true},
		{"unrelated kind: lowercase 'foo' reported", "foo", "type", false},
		// non-Go-named entry: function named 'main' is filtered regardless of casing.
		{"function literally named 'init' excluded", "init", "method", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeDeadQuerier{
				dead: []ports.NodeRef{
					{NodeID: "n-x", FilePath: "x.go", Kind: tc.kind, Name: tc.nodeName, LineStart: 1, LineEnd: 2},
				},
			}
			c := checks.NewDeadCodeCheck(q)
			findings, err := c.Run(context.Background(), checks.Input{
				RepoID: "r", Branch: "main", FilePaths: []string{"x.go"},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantFilter && len(findings) != 0 {
				t.Errorf("expected node to be filtered, got %d findings", len(findings))
			}
			if !tc.wantFilter && len(findings) != 1 {
				t.Errorf("expected node to produce 1 finding, got %d", len(findings))
			}
		})
	}
}

// 7. Empty FilePaths input -> no findings, no panic, querier not invoked with surprises.
func TestDeadCodeCheck_EmptyFilePathsIsNoOp(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{{NodeID: "n", FilePath: "a.go", Kind: "function", Name: "helper"}},
	}
	c := checks.NewDeadCodeCheck(q)
	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: nil,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected zero findings for empty FilePaths, got %d", len(findings))
	}
}

func TestDeadCodeCheck_QuerierErrorPropagates(t *testing.T) {
	want := errors.New("db down")
	q := &fakeDeadQuerier{err: want}
	c := checks.NewDeadCodeCheck(q)
	_, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"a.go"},
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestDeadCodeCheck_ThreadsContentHash verifies that the check carries the
// dead node's nodes.content_hash onto the emitted finding so the revalidation
// sweep can detect drift.
func TestDeadCodeCheck_ThreadsContentHash(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{
			{NodeID: "n-dead", FilePath: "pkg/a.go", Kind: "function", Name: "helper", ContentHash: "h-abc"},
		},
	}
	c := checks.NewDeadCodeCheck(q)
	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"pkg/a.go"},
	})
	if err != nil || len(findings) != 1 {
		t.Fatalf("Run: err=%v len=%d", err, len(findings))
	}
	if findings[0].AnchorContentHash == nil {
		t.Fatal("AnchorContentHash is nil")
	}
	if *findings[0].AnchorContentHash != "h-abc" {
		t.Errorf("AnchorContentHash = %q, want h-abc", *findings[0].AnchorContentHash)
	}
}

// TestDeadCodeCheck_EmptyContentHashStaysNil verifies a NodeRef carrying an
// empty content hash (older pre-content-hash rows) leaves AnchorContentHash nil.
func TestDeadCodeCheck_EmptyContentHashStaysNil(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{
			{NodeID: "n-dead", FilePath: "pkg/a.go", Kind: "function", Name: "helper", ContentHash: ""},
		},
	}
	c := checks.NewDeadCodeCheck(q)
	findings, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"pkg/a.go"},
	})
	if err != nil || len(findings) != 1 {
		t.Fatalf("Run: err=%v len=%d", err, len(findings))
	}
	if findings[0].AnchorContentHash != nil {
		t.Errorf("AnchorContentHash should stay nil for empty hash, got %q",
			*findings[0].AnchorContentHash)
	}
}

// 4. Branch-stable finding_id is deterministic per (rule, anchor).
func TestDeadCodeCheck_DeterministicFindingID(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{
			{NodeID: "n-same", FilePath: "a.go", Kind: "function", Name: "helper"},
		},
	}
	c := checks.NewDeadCodeCheck(q)
	in := checks.Input{RepoID: "r", Branch: "main", FilePaths: []string{"a.go"}}

	first, err := c.Run(context.Background(), in)
	if err != nil || len(first) != 1 {
		t.Fatalf("first run: err=%v len=%d", err, len(first))
	}
	second, err := c.Run(context.Background(), in)
	if err != nil || len(second) != 1 {
		t.Fatalf("second run: err=%v len=%d", err, len(second))
	}
	if first[0].FindingID != second[0].FindingID {
		t.Errorf("FindingID not stable across runs: %q vs %q", first[0].FindingID, second[0].FindingID)
	}
}
