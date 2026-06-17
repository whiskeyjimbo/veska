// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package checks_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeDeadQuerier returns a preconfigured list of "dead" nodes regardless of
// inputs, and remembers the last call args for assertions.
type fakeDeadQuerier struct {
	dead         []ports.NodeRef
	ifaceMethods []string
	err          error
	gotRepo      string
	gotBranch    string
	gotPaths     []string
	callCount    int
}

func (f *fakeDeadQuerier) InterfaceMethodNames(_ context.Context, _, _ string) ([]string, error) {
	return f.ifaceMethods, nil
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
		// 'type', 'struct', 'interface' kinds are no longer
		// in deadCodeKinds - a CALLS-based liveness test is meaningless
		// for non-callable kinds. Both cases must filter regardless of
		// name casing.
		{"type kind excluded (non-callable, post f1zp)", "Foo", "type", true},
		{"type kind excluded - lowercase too (post f1zp)", "foo", "type", true},
		{"struct kind excluded (post f1zp)", "boolValue", "struct", true},
		{"interface kind excluded (post f1zp)", "Value", "interface", true},
		// non-Go-named entry: function named 'main' is filtered regardless of casing.
		{"function literally named 'init' excluded", "init", "method", true},
		// Non-symbol kinds carry no inbound edges by construction and must
		// never be reported, regardless of name casing.
		{"package kind excluded", "server", "package", true},
		{"chunk kind excluded", "chunk:1-4", "chunk", true},
		{"file kind excluded", "main.go", "file", true},
		{"field kind excluded", "addr", "field", true},
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

// TestDeadCodeCheck_SkipsTestFiles pins: lowercase symbols in
// test-file conventions (Go _test.go, pytest test_*.py, jest *.test.ts, …)
// must not produce dead-code findings, since test helpers are often passed
// as function values or referenced only by their test, neither of which
// produces a CALLS edge today.
func TestDeadCodeCheck_SkipsTestFiles(t *testing.T) {
	cases := []struct {
		name       string
		filePath   string
		wantFilter bool
	}{
		{"go _test.go skipped", "internal/cli/groups_test.go", true},
		{"absolute go _test.go skipped", "/repo/internal/cli/groups_test.go", true},
		{"pytest test_ prefix skipped", "tests/test_loader.py", true},
		{"pytest _test suffix skipped", "tests/loader_test.py", true},
		{"jest .test.ts skipped", "src/foo.test.ts", true},
		{"jest .test.jsx skipped", "src/Foo.test.jsx", true},
		{"jasmine .spec.ts skipped", "src/foo.spec.ts", true},
		{"plain .go reported", "internal/cli/groups.go", false},
		{"plain .py reported", "lib/loader.py", false},
		{"plain .ts reported", "src/foo.ts", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeDeadQuerier{
				dead: []ports.NodeRef{
					{NodeID: "n-x", FilePath: tc.filePath, Kind: "function", Name: "helper", LineStart: 1, LineEnd: 2},
				},
			}
			c := checks.NewDeadCodeCheck(q)
			findings, err := c.Run(context.Background(), checks.Input{
				RepoID: "r", Branch: "main", FilePaths: []string{tc.filePath},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantFilter && len(findings) != 0 {
				t.Errorf("expected %s to be filtered, got %d findings", tc.filePath, len(findings))
			}
			if !tc.wantFilter && len(findings) != 1 {
				t.Errorf("expected %s to produce 1 finding, got %d", tc.filePath, len(findings))
			}
		})
	}
}

// TestDeadCodeCheck_SkipsVendoredFiles pins: dead-code must not
// fire on vendored deps. The path predicate is shared with secret_leak and
// auto-link via pathfilter.IsVendored.
func TestDeadCodeCheck_SkipsVendoredFiles(t *testing.T) {
	cases := []struct {
		path       string
		wantFilter bool
	}{
		{"vendor/github.com/spf13/cobra/cobra.go", true},
		{"node_modules/lodash/index.js", true},
		{"third_party/protobuf/x.proto", true},
		{"apps/cli/vendor/github.com/spf13/pflag/x.go", true},
		{"internal/cli/groups.go", false},
		{"vendored_data/keys.go", false}, // substring; must not filter
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			q := &fakeDeadQuerier{
				dead: []ports.NodeRef{
					{NodeID: "n-x", FilePath: tc.path, Kind: "function", Name: "helper", LineStart: 1, LineEnd: 2},
				},
			}
			c := checks.NewDeadCodeCheck(q)
			findings, err := c.Run(context.Background(), checks.Input{
				RepoID: "r", Branch: "main", FilePaths: []string{tc.path},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.wantFilter && len(findings) != 0 {
				t.Errorf("%s: want filtered, got %d findings", tc.path, len(findings))
			}
			if !tc.wantFilter && len(findings) != 1 {
				t.Errorf("%s: want 1 finding, got %d", tc.path, len(findings))
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
		return
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
		return
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

// TestDeadCodeCheck_SkipsEphemeralRepo pins: an external
// cache-tier clone (kind == "ephemeral", e.g. a repo registered by
// `veska search --repo <url>`) must not produce dead-code findings. A 75
// file pflag-like clone otherwise emits a wall of low-severity findings on
// every unused exported helper that is part of the upstream public API,
// training juniors to ignore the findings surface on day one. Mirrors the
// autolink short-circuit added in.
func TestDeadCodeCheck_SkipsEphemeralRepo(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{
			{NodeID: "n-helper", FilePath: "pkg/a.go", Kind: "function", Name: "helper", LineStart: 1, LineEnd: 2},
		},
	}
	lookup := func(_ context.Context, repoID string) (string, error) {
		if repoID == "ephem-repo" {
			return "ephemeral", nil
		}
		return "tracked", nil
	}
	c := checks.NewDeadCodeCheck(q, checks.WithDeadCodeRepoKindLookup(lookup))

	got, err := c.Run(context.Background(), checks.Input{
		RepoID: "ephem-repo", Branch: "main", FilePaths: []string{"pkg/a.go"},
	})
	if err != nil {
		t.Fatalf("Run(ephemeral): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ephemeral repo: want 0 findings, got %d", len(got))
	}
	if q.callCount != 0 {
		t.Errorf("querier should not be invoked for ephemeral repo, got callCount=%d", q.callCount)
	}

	got, err = c.Run(context.Background(), checks.Input{
		RepoID: "tracked-repo", Branch: "main", FilePaths: []string{"pkg/a.go"},
	})
	if err != nil {
		t.Fatalf("Run(tracked): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("tracked repo: want 1 finding, got %d", len(got))
	}
}

// TestDeadCodeCheck_RepoKindLookupErrorFailsOpen verifies that a lookup
// error does not suppress findings - we'd rather over-report on a registry
// glitch than silently skip dead-code on a tracked repo.
func TestDeadCodeCheck_RepoKindLookupErrorFailsOpen(t *testing.T) {
	q := &fakeDeadQuerier{
		dead: []ports.NodeRef{
			{NodeID: "n-helper", FilePath: "pkg/a.go", Kind: "function", Name: "helper"},
		},
	}
	lookup := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("registry down")
	}
	c := checks.NewDeadCodeCheck(q, checks.WithDeadCodeRepoKindLookup(lookup))
	got, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"pkg/a.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("lookup error: want 1 finding (fail-open), got %d", len(got))
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

// TestDeadCodeCheck_SkipsInterfaceMethodImplementations covers
// concrete methods whose bare name matches a same-repo
// interface method are interface implementations called via dispatch
// (the static graph cannot see those edges) and must not be flagged
// dead. The pflag junior-journey concrete: boolValue.Set /.String /
// Type satisfy pflag.Value, but they have zero inbound CALLS edges
// in the graph, so before the fix all three got flagged. Methods
// whose bare name is NOT an interface method (boolValue.privateHelper)
// still get reported when otherwise dead.
func TestDeadCodeCheck_SkipsInterfaceMethodImplementations(t *testing.T) {
	q := &fakeDeadQuerier{
		// Same shape pflag produced: lowercase concrete struct
		// methods that look 'dead' but satisfy the Value interface.
		dead: []ports.NodeRef{
			{NodeID: "n1", FilePath: "bool.go", Kind: "method", Name: "boolValue.Set"},
			{NodeID: "n2", FilePath: "bool.go", Kind: "method", Name: "boolValue.String"},
			{NodeID: "n3", FilePath: "bool.go", Kind: "method", Name: "boolValue.Type"},
			{NodeID: "n4", FilePath: "bool.go", Kind: "method", Name: "boolValue.privateHelper"},
		},
		ifaceMethods: []string{"Set", "String", "Type", "Replace"},
	}
	c := checks.NewDeadCodeCheck(q)
	got, err := c.Run(context.Background(),
		checks.Input{RepoID: "r", Branch: "main", FilePaths: []string{"bool.go"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only the helper survives - its bare name "privateHelper" isn't an
	// interface method declared in this repo.
	if len(got) != 1 {
		names := make([]string, 0, len(got))
		for _, f := range got {
			names = append(names, f.Message)
		}
		t.Fatalf("expected 1 finding (boolValue.privateHelper); got %d: %v", len(got), names)
	}
	if !strings.Contains(got[0].Message, "privateHelper") {
		t.Errorf("expected the privateHelper finding, got %q", got[0].Message)
	}
}
