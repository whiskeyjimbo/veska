package wiki

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// epFixtureGraph builds a promoted state covering the four ranking
// signals (solov2-73f): inbound count, exported flag, has-adjacent-test
// tiebreaker, and the test-symbol exclusion gate.
//
// Nodes:
//
//	Hub         high fan-in (10 callers), exported, has _test caller   -> top
//	lowFanCaps  one caller, exported, has _test caller                  -> middle
//	helper      one caller (non-test), unexported, no test caller       -> low
//	flagged     would qualify but has an open finding                   -> EXCLUDED
//	TestThing   Test-prefixed symbol                                    -> EXCLUDED by gate
func epFixtureGraph(t *testing.T) *domain.Graph {
	t.Helper()
	g, err := domain.NewGraph("r1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	mk := func(id, path string, kind domain.NodeKind) {
		n, err := domain.NewNode(id, path, id, kind)
		if err != nil {
			t.Fatalf("NewNode %s: %v", id, err)
		}
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
	}
	mk("Hub", "app/hub.go", domain.KindFunction)
	mk("lowFanCaps", "app/low.go", domain.KindFunction)
	mk("helper", "app/util.go", domain.KindFunction)
	mk("flagged", "app/flagged.go", domain.KindFunction)
	mk("TestThing", "app/x.go", domain.KindFunction)

	mk("hub_test_caller", "app/hub_test.go", domain.KindTest)
	mk("low_test_caller", "app/low_test.go", domain.KindTest)
	mk("prod_caller", "app/main.go", domain.KindFunction)
	mk("flagged_test", "app/flagged_test.go", domain.KindTest)
	for i := 1; i <= 9; i++ {
		mk("hubCaller"+string(rune('0'+i)), "app/c.go", domain.KindFunction)
	}
	return g
}

func epFixtureInbound() map[string][]string {
	hub := []string{"hub_test_caller"}
	for i := 1; i <= 9; i++ {
		hub = append(hub, "hubCaller"+string(rune('0'+i)))
	}
	return map[string][]string{
		"Hub":        hub,
		"lowFanCaps": {"low_test_caller"},
		"helper":     {"prod_caller"},
		"flagged":    {"flagged_test"},
		"TestThing":  {"prod_caller"},
	}
}

func epFixtureService(t *testing.T, opts ...EntryPointOption) *EntryPointsService {
	t.Helper()
	g := epFixtureGraph(t)
	loadGraph := func(_ context.Context, _, _ string) (*domain.Graph, error) { return g, nil }
	inbound := epFixtureInbound()
	inboundEdges := func(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
		out := make(map[string][]string, len(ids))
		for _, id := range ids {
			out[id] = append([]string(nil), inbound[id]...)
		}
		return out, nil
	}
	openFindings := func(_ context.Context, _, _ string) (map[string]bool, error) {
		return map[string]bool{"flagged": true}, nil
	}
	svc, err := NewEntryPointsService(loadGraph, inboundEdges, openFindings, opts...)
	if err != nil {
		t.Fatalf("NewEntryPointsService: %v", err)
	}
	return svc
}

func TestNewEntryPointsService_RejectsNilDependencies(t *testing.T) {
	lg := func(context.Context, string, string) (*domain.Graph, error) { return nil, nil }
	ie := func(context.Context, string, string, []string) (map[string][]string, error) { return nil, nil }
	of := func(context.Context, string, string) (map[string]bool, error) { return nil, nil }

	if _, err := NewEntryPointsService(nil, ie, of); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil loadGraph: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewEntryPointsService(lg, nil, of); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil inboundEdges: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewEntryPointsService(lg, ie, nil); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil openFindings: want ErrMissingDependency, got %v", err)
	}
}

// TestSelect_RanksByInboundCountFirst pins the primary ranking signal:
// the highest-fan-in symbol wins regardless of any other property.
func TestSelect_RanksByInboundCountFirst(t *testing.T) {
	svc := epFixtureService(t)
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(rep.EntryPoints) == 0 || rep.EntryPoints[0].SymbolName != "Hub" {
		var names []string
		for _, ep := range rep.EntryPoints {
			names = append(names, ep.SymbolName)
		}
		t.Fatalf("expected Hub first by inbound count; got %v", names)
	}
	if rep.EntryPoints[0].InboundCount != 10 {
		t.Errorf("Hub: expected inbound_count=10, got %d", rep.EntryPoints[0].InboundCount)
	}
}

// TestSelect_ExportedBeatsUnexportedOnTie pins tiebreaker #2: when
// inbound counts match, the exported (capitalised) symbol wins.
func TestSelect_ExportedBeatsUnexportedOnTie(t *testing.T) {
	svc := epFixtureService(t)
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// lowFanCaps and helper both have InboundCount=1 (1 caller each).
	// lowFanCaps is exported, helper is not — lowFanCaps must rank higher.
	var lowIdx, helperIdx = -1, -1
	for i, ep := range rep.EntryPoints {
		switch ep.SymbolName {
		case "lowFanCaps":
			lowIdx = i
		case "helper":
			helperIdx = i
		}
	}
	if lowIdx < 0 || helperIdx < 0 {
		t.Fatalf("missing entries: %+v", rep.EntryPoints)
	}
	if lowIdx > helperIdx {
		t.Errorf("expected lowFanCaps (exported) before helper (unexported); got positions %d,%d",
			lowIdx, helperIdx)
	}
}

// TestSelect_FlaggedNodesExcluded keeps the open-finding gate.
func TestSelect_FlaggedNodesExcluded(t *testing.T) {
	svc := epFixtureService(t)
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for _, ep := range rep.EntryPoints {
		if ep.SymbolName == "flagged" {
			t.Errorf("flagged node must be excluded by open-finding gate")
		}
	}
}

// TestSelect_TestSymbolsExcludedByDefault pins solov2-m8d: Test-prefixed
// symbols and symbols in *_test.go files stay out unless IncludeTests=true.
func TestSelect_TestSymbolsExcludedByDefault(t *testing.T) {
	svc := epFixtureService(t)
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for _, ep := range rep.EntryPoints {
		if ep.SymbolName == "TestThing" {
			t.Errorf("Test-prefixed symbol must be excluded by default")
		}
	}
	rep2, _ := svc.SelectWith(context.Background(), "r1", "main", SelectOptions{IncludeTests: true})
	have := map[string]bool{}
	for _, ep := range rep2.EntryPoints {
		have[ep.SymbolName] = true
	}
	if !have["TestThing"] {
		t.Errorf("IncludeTests=true should surface TestThing; got %+v", rep2.EntryPoints)
	}
}

// TestSelect_AdjacentTestIsTiebreakerNotGate pins the bead's key
// behavior change: a symbol without an adjacent test STILL appears
// (used to be excluded), it just ranks below an equivalent symbol
// that does have one.
func TestSelect_AdjacentTestIsTiebreakerNotGate(t *testing.T) {
	svc := epFixtureService(t)
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	have := map[string]bool{}
	for _, ep := range rep.EntryPoints {
		have[ep.SymbolName] = true
	}
	// helper has no adjacent test — must STILL appear (no hard gate).
	if !have["helper"] {
		t.Errorf("helper (no adjacent test) must still appear under new ranking; got %+v",
			rep.EntryPoints)
	}
}

// TestRenderEntryPoints_Deterministic AC: rendering the same promoted
// state twice yields byte-identical output.
func TestRenderEntryPoints_Deterministic(t *testing.T) {
	svc := epFixtureService(t)
	rep1, _ := svc.Select(context.Background(), "r1", "main")
	rep2, _ := svc.Select(context.Background(), "r1", "main")
	out1 := RenderEntryPoints(rep1)
	out2 := RenderEntryPoints(rep2)
	if out1 != out2 {
		t.Errorf("render not byte-identical:\n--- 1 ---\n%s\n--- 2 ---\n%s", out1, out2)
	}
	if !strings.Contains(out1, "Hub") || !strings.Contains(out1, "Inbound") {
		t.Errorf("rendered page missing expected content:\n%s", out1)
	}
}

func TestRenderEntryPoints_EmptyReport(t *testing.T) {
	out := RenderEntryPoints(EntryPointsReport{RepoID: "r1", Branch: "main"})
	if !strings.Contains(out, "No entry points") {
		t.Errorf("empty report should render a placeholder, got:\n%s", out)
	}
}

func TestEntryPointsPagePath_IsUnderDocsVeska(t *testing.T) {
	if EntryPointsPagePath != "docs/veska/entry_points.md" {
		t.Errorf("unexpected page path %q", EntryPointsPagePath)
	}
}

// TestEntryPointsService_FiltersGoInitFuncs pins solov2-q5gd: Go's runtime
// invokes every package-scoped init() automatically, so they aren't
// "places to start reading" from an agent's perspective. cobra-style CLIs
// register every subcommand inside init(), which previously dominated the
// entry_points page on small repos (one init() per command file ranked
// above main() and Execute()).
func TestEntryPointsService_FiltersGoInitFuncs(t *testing.T) {
	g, err := domain.NewGraph("r1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	mk := func(id, name, path string, kind domain.NodeKind) {
		n, err := domain.NewNode(id, path, name, kind)
		if err != nil {
			t.Fatalf("NewNode %s: %v", id, err)
		}
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
	}
	mk("root-init", "init", "cmd/root.go", domain.KindFunction)
	mk("token-init", "init", "cmd/token.go", domain.KindFunction)
	mk("Execute", "Execute", "cmd/root.go", domain.KindFunction)
	// Python __init__ must NOT be filtered — it is a constructor, not a
	// Go-runtime hook. Keep one in the fixture to lock that contract.
	mk("py-init", "__init__", "app/server.py", domain.KindFunction)

	inbound := map[string][]string{
		"root-init":  {"a", "b", "c"}, // high fan-in, would have ranked top
		"token-init": {"a", "b"},
		"Execute":    {"main-caller"},
		"py-init":    {"caller"},
	}
	svc, err := NewEntryPointsService(
		func(context.Context, string, string) (*domain.Graph, error) { return g, nil },
		func(_ context.Context, _, _ string, _ []string) (map[string][]string, error) { return inbound, nil },
		func(context.Context, string, string) (map[string]bool, error) { return nil, nil },
	)
	if err != nil {
		t.Fatalf("NewEntryPointsService: %v", err)
	}
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for _, ep := range rep.EntryPoints {
		if ep.Name == "init" && strings.HasSuffix(ep.FilePath, ".go") {
			t.Errorf("Go init() leaked into entry_points: %+v", ep)
		}
	}
	// Sanity: Python __init__ still present, exported Execute still present.
	hasPyInit, hasExecute := false, false
	for _, ep := range rep.EntryPoints {
		if ep.Name == "__init__" {
			hasPyInit = true
		}
		if ep.Name == "Execute" {
			hasExecute = true
		}
	}
	if !hasPyInit {
		t.Errorf("Python __init__ was filtered; only Go init() should be")
	}
	if !hasExecute {
		t.Errorf("Execute missing — Go init() filter is too aggressive")
	}
}

func TestIsExported(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Foo", true},
		{"foo", false},
		{"", false},
		{"Bar123", true},
		{"_foo", false},
	}
	for _, c := range cases {
		if got := isExported(c.name); got != c.want {
			t.Errorf("isExported(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
