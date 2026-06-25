// SPDX-License-Identifier: AGPL-3.0-only

package checks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
)

type fakePkgGraph struct {
	graph map[string][]string
	err   error
}

func (f fakePkgGraph) PackageDependencies(_ context.Context, _, _ string) (map[string][]string, error) {
	return f.graph, f.err
}

func TestImportCycleCheck_FlagsCycle(t *testing.T) {
	// a -> b -> c -> a is one cycle; d -> a is acyclic and must not be flagged.
	g := fakePkgGraph{graph: map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
		"d": {"a"},
	}}
	c := checks.NewImportCycleCheck(g)
	findings, err := c.Run(context.Background(), checks.Input{RepoID: "r1", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Rule != "import-cycle" {
		t.Errorf("rule = %q, want import-cycle", f.Rule)
	}
	if f.FilePath == nil || *f.FilePath != "a" {
		t.Errorf("anchor = %v, want a (smallest member)", f.FilePath)
	}
	for _, m := range []string{"a", "b", "c"} {
		if !strings.Contains(f.Message, m) {
			t.Errorf("message %q missing cycle member %q", f.Message, m)
		}
	}
	if strings.Contains(f.Message, "d ") {
		t.Errorf("acyclic package d leaked into the cycle message: %q", f.Message)
	}
}

func TestImportCycleCheck_StableIDAcrossRuns(t *testing.T) {
	g := fakePkgGraph{graph: map[string][]string{"x": {"y"}, "y": {"x"}}}
	c := checks.NewImportCycleCheck(g)
	run := func() string {
		fs, err := c.Run(context.Background(), checks.Input{RepoID: "r1", Branch: "main"})
		if err != nil || len(fs) != 1 {
			t.Fatalf("run: err=%v len=%d", err, len(fs))
		}
		return fs[0].FindingID
	}
	if a, b := run(), run(); a != b {
		t.Errorf("finding_id not stable across runs: %q vs %q", a, b)
	}
}

func TestImportCycleCheck_NoCycle(t *testing.T) {
	g := fakePkgGraph{graph: map[string][]string{"a": {"b"}, "b": {"c"}}}
	c := checks.NewImportCycleCheck(g)
	findings, err := c.Run(context.Background(), checks.Input{RepoID: "r1", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("acyclic graph: got %d findings, want 0", len(findings))
	}
}

func TestImportCycleCheck_SkipsEphemeral(t *testing.T) {
	g := fakePkgGraph{graph: map[string][]string{"a": {"b"}, "b": {"a"}}}
	c := checks.NewImportCycleCheck(g, checks.WithImportCycleRepoKindLookup(
		func(context.Context, string) (string, error) { return "ephemeral", nil },
	))
	findings, err := c.Run(context.Background(), checks.Input{RepoID: "r1", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("ephemeral repo: got %d findings, want 0", len(findings))
	}
}

func TestImportCycleCheck_AuthoritativeRule(t *testing.T) {
	c := checks.NewImportCycleCheck(fakePkgGraph{})
	rule, ok := c.AuthoritativeRule(checks.Input{})
	if !ok || rule != "import-cycle" {
		t.Errorf("AuthoritativeRule = (%q,%v), want (import-cycle,true)", rule, ok)
	}
}
