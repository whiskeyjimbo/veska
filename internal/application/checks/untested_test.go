package checks_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeCoverageQuerier returns a preconfigured candidate list, filtered to the
// requested file paths so empty-paths semantics are honoured.
type fakeCoverageQuerier struct {
	nodes     []ports.NodeCallers
	err       error
	gotRepo   string
	gotBranch string
	gotPaths  []string
}

func (f *fakeCoverageQuerier) CandidateCallersInFiles(_ context.Context, repoID, branch string, filePaths []string) ([]ports.NodeCallers, error) {
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
	var out []ports.NodeCallers
	for _, n := range f.nodes {
		if _, ok := allow[n.Node.FilePath]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func nc(nodeID, file, kind, name string, callers ...string) ports.NodeCallers {
	return ports.NodeCallers{
		Node:        ports.NodeRef{NodeID: nodeID, FilePath: file, Kind: kind, Name: name},
		CallerFiles: callers,
	}
}

func TestUntestedCheck_Name(t *testing.T) {
	c := checks.NewUntestedSymbolCheck(&fakeCoverageQuerier{})
	if c.Name() != "untested-symbol" {
		t.Errorf("Name() = %q, want %q", c.Name(), "untested-symbol")
	}
}

func TestUntestedCheck_ImplementsCheck(t *testing.T) {
	var _ checks.Check = checks.NewUntestedSymbolCheck(&fakeCoverageQuerier{})
}

// AC1: a prod symbol with no test-file caller is emitted as a finding.
func TestUntestedCheck_FlagsSymbolWithNoTestCaller(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		// called only from a prod file -> untested
		nc("n1", "internal/svc/svc.go", "function", "doWork", "internal/svc/handler.go"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d findings, want 1", len(out))
	}
	f := out[0]
	if f.Rule != "untested-symbol" {
		t.Errorf("rule = %q, want untested-symbol", f.Rule)
	}
	if f.NodeID == nil || *f.NodeID != "n1" {
		t.Errorf("node anchor = %v, want n1", f.NodeID)
	}
	if !strings.Contains(f.Message, "doWork") {
		t.Errorf("message %q missing symbol name", f.Message)
	}
}

// the finding carries the symbol's content-hash as its anchor,
// so the revalidation sweep selects it on body drift and re-runs the test-caller
// predicate (rather than the old no-anchor behaviour that excluded it).
func TestUntestedCheck_EmitsAnchorContentHash(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		{
			Node:        ports.NodeRef{NodeID: "n1", FilePath: "internal/svc/svc.go", Kind: "function", Name: "doWork", ContentHash: "h-body"},
			CallerFiles: []string{"internal/svc/handler.go"}, // prod caller only
		},
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d findings, want 1", len(out))
	}
	if out[0].AnchorContentHash == nil || *out[0].AnchorContentHash != "h-body" {
		t.Errorf("anchor content-hash = %v, want h-body", out[0].AnchorContentHash)
	}
}

// AC2: a prod symbol with >=1 test-file caller produces no finding.
func TestUntestedCheck_NoFindingWhenTestCallerPresent(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/svc.go", "function", "doWork",
			"internal/svc/handler.go", "internal/svc/svc_test.go"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d findings, want 0", len(out))
	}
}

// The trap: an EXPORTED untested symbol is the highest-value finding, NOT a
// false positive - unlike dead-code, a test caller is always a visible CALLS
// edge, so the dead-code exported-symbol exclusion must NOT be carried here.
func TestUntestedCheck_FlagsExportedSymbol(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/api.go", "function", "DoPublicThing", "internal/svc/caller.go"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/api.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("exported untested symbol: got %d findings, want 1", len(out))
	}
}

// A zero-caller node (no callers at all) is still untested - intentional
// overlap with dead-code; the freshly-added exported API with no caller yet
// is the highest-value case and must not be suppressed.
func TestUntestedCheck_FlagsZeroCallerSymbol(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/api.go", "function", "BrandNew"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/api.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("zero-caller symbol: got %d findings, want 1", len(out))
	}
}

// Symbols DEFINED in a test file are not prod symbols - never flagged.
func TestUntestedCheck_SkipsTestFileSymbols(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/svc_test.go", "function", "helperFn"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, _ := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc_test.go"},
	})
	if len(out) != 0 {
		t.Fatalf("test-file symbol: got %d findings, want 0", len(out))
	}
}

// Non-callable kinds (package/file/field/struct) carry no CALLS liveness signal
// and must be excluded, same as dead-code.
func TestUntestedCheck_SkipsNonCallableKinds(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/svc.go", "struct", "Config"),
		nc("n2", "internal/svc/svc.go", "field", "Timeout"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, _ := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc.go"},
	})
	if len(out) != 0 {
		t.Fatalf("non-callable kinds: got %d findings, want 0", len(out))
	}
}

// Entry points (main/init) are expected-untested noise -> excluded.
func TestUntestedCheck_SkipsEntryPoints(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "cmd/app/main.go", "function", "main"),
		nc("n2", "internal/svc/svc.go", "function", "init"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, _ := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"cmd/app/main.go", "internal/svc/svc.go"},
	})
	if len(out) != 0 {
		t.Fatalf("entry points: got %d findings, want 0", len(out))
	}
}

// Ephemeral repos (cache-tier clones) skip entirely - mirrors dead-code's
// izh6.13 short-circuit; untested-symbol has strictly worse exposure on an
// external library's public API.
func TestUntestedCheck_SkipsEphemeralRepo(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/svc.go", "function", "doWork"),
	}}
	c := checks.NewUntestedSymbolCheck(q, checks.WithUntestedRepoKindLookup(
		func(context.Context, string) (string, error) { return "ephemeral", nil },
	))
	out, err := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc.go"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("ephemeral repo: got %d findings, want 0", len(out))
	}
}

// A tracked repo (or a lookup error - fail open) still reports.
func TestUntestedCheck_TrackedRepoReports(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/svc.go", "function", "doWork"),
	}}
	c := checks.NewUntestedSymbolCheck(q, checks.WithUntestedRepoKindLookup(
		func(context.Context, string) (string, error) { return "tracked", nil },
	))
	out, _ := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/svc.go"},
	})
	if len(out) != 1 {
		t.Fatalf("tracked repo: got %d findings, want 1", len(out))
	}
}

type fakeIfaceLister struct{ names []string }

func (f fakeIfaceLister) InterfaceMethodNames(context.Context, string, string) ([]string, error) {
	return f.names, nil
}

// A concrete method whose bare name matches a same-repo interface method is
// suppressed (interface-dispatch proxy blind spot) - the persona-test fix.
func TestUntestedCheck_SuppressesInterfaceMethodImpl(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/en.go", "method", "EN.Greet"), // no test caller
	}}
	c := checks.NewUntestedSymbolCheck(q,
		checks.WithUntestedInterfaceMethods(fakeIfaceLister{names: []string{"Greet"}}))
	out, _ := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/en.go"},
	})
	if len(out) != 0 {
		t.Fatalf("interface-method impl must be suppressed; got %d findings", len(out))
	}
}

// A non-interface method with no test caller is still flagged (suppression is
// keyed on the interface-method name set).
func TestUntestedCheck_NonInterfaceMethodStillFlagged(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/en.go", "method", "EN.Helper"),
	}}
	c := checks.NewUntestedSymbolCheck(q,
		checks.WithUntestedInterfaceMethods(fakeIfaceLister{names: []string{"Greet"}}))
	out, _ := c.Run(context.Background(), checks.Input{
		RepoID: "r", Branch: "main", FilePaths: []string{"internal/svc/en.go"},
	})
	if len(out) != 1 {
		t.Fatalf("non-interface method should still flag; got %d", len(out))
	}
}

func TestUntestedCheck_EmptyFilePathsNoOp(t *testing.T) {
	q := &fakeCoverageQuerier{nodes: []ports.NodeCallers{
		nc("n1", "internal/svc/svc.go", "function", "doWork"),
	}}
	c := checks.NewUntestedSymbolCheck(q)
	out, err := c.Run(context.Background(), checks.Input{RepoID: "r", Branch: "main", FilePaths: nil})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty filepaths: got %d findings, want 0", len(out))
	}
}

func TestUntestedCheck_QuerierErrorPropagates(t *testing.T) {
	q := &fakeCoverageQuerier{err: errors.New("boom")}
	c := checks.NewUntestedSymbolCheck(q)
	_, err := c.Run(context.Background(), checks.Input{RepoID: "r", Branch: "main", FilePaths: []string{"x.go"}})
	if err == nil {
		t.Fatal("expected error from querier to propagate")
	}
}

func TestUntestedCheck_NilQuerier(t *testing.T) {
	c := checks.NewUntestedSymbolCheck(nil)
	_, err := c.Run(context.Background(), checks.Input{RepoID: "r", Branch: "main", FilePaths: []string{"x.go"}})
	if err == nil {
		t.Fatal("expected error from nil querier")
	}
}
