package wiki

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// epFakeNodes is a NodeLookup fixture for the blastradius service used by
// the entry_points tests.
type epFakeNodes struct {
	metas map[string]ports.NodeMeta
}

func (f *epFakeNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	var out []ports.NodeMeta
	for _, id := range ids {
		if m, ok := f.metas[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *epFakeNodes) NodesInFile(_ context.Context, _, _, _ string) ([]string, error) {
	return nil, nil
}

// heavyCallerIDs are the inbound callers of "heavy". There are eleven, so
// the blast radius of "heavy" (seed + 11 callers = 12) exceeds the default
// ceiling of 10 — "heavy" only qualifies once the cap is raised.
var heavyCallerIDs = []string{
	"heavy_test", "h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8", "h9", "h10",
}

// epFixtureGraph builds a promoted state with four function symbols:
//
//	low      app/low.go     — inbound caller from low_test.go, radius 2, no finding -> CANDIDATE
//	heavy    app/heavy.go   — adjacent test, but blast radius 12 (over default K)   -> excluded by default
//	untested app/untested.go— no test caller                                       -> excluded
//	flagged  app/flagged.go — adjacent test, small radius, but an open finding      -> excluded
func epFixtureGraph(t *testing.T) *domain.Graph {
	t.Helper()
	g, err := domain.NewGraph("r1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	mk := func(id, path string, kind domain.NodeKind) *domain.Node {
		n, err := domain.NewNode(id, path, id, kind)
		if err != nil {
			t.Fatalf("NewNode %s: %v", id, err)
		}
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", id, err)
		}
		return n
	}
	mk("low", "app/low.go", domain.KindFunction)
	mk("heavy", "app/heavy.go", domain.KindFunction)
	mk("untested", "app/untested.go", domain.KindFunction)
	mk("flagged", "app/flagged.go", domain.KindFunction)
	// test callers
	mk("low_test", "app/low_test.go", domain.KindTest)
	mk("heavy_test", "app/heavy_test.go", domain.KindTest)
	mk("flagged_test", "app/flagged_test.go", domain.KindTest)
	// non-test caller for "untested"
	mk("prod_caller", "app/main.go", domain.KindFunction)
	// blast-radius padding for "heavy": ten downstream dependents
	for i := 1; i <= 10; i++ {
		mk(fmt.Sprintf("h%d", i), fmt.Sprintf("app/h%d.go", i), domain.KindFunction)
	}
	return g
}

func epFixtureService(t *testing.T, opts ...EntryPointOption) *EntryPointsService {
	t.Helper()
	g := epFixtureGraph(t)
	loadGraph := func(_ context.Context, _, _ string) (*domain.Graph, error) { return g, nil }

	// inbound adjacency keyed by target node.
	inbound := map[string][]string{
		"low":      {"low_test"},
		"heavy":    heavyCallerIDs,
		"untested": {"prod_caller"},
		"flagged":  {"flagged_test"},
	}
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
	metas := map[string]ports.NodeMeta{
		"low": {NodeID: "low"}, "heavy": {NodeID: "heavy"},
		"untested": {NodeID: "untested"}, "flagged": {NodeID: "flagged"},
		"heavy_test": {NodeID: "heavy_test"},
		"low_test":   {NodeID: "low_test"}, "flagged_test": {NodeID: "flagged_test"},
		"prod_caller": {NodeID: "prod_caller"},
	}
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("h%d", i)
		metas[id] = ports.NodeMeta{NodeID: id}
	}
	blast := blastradius.NewService(&epFakeEdges{inbound: inbound}, &epFakeNodes{metas: metas}, nil)

	svc, err := NewEntryPointsService(loadGraph, inboundEdges, openFindings, blast, opts...)
	if err != nil {
		t.Fatalf("NewEntryPointsService: %v", err)
	}
	return svc
}

// epFakeEdges is an EdgeReader fixture for the blastradius service.
type epFakeEdges struct {
	inbound map[string][]string
}

func (f *epFakeEdges) InboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.inbound[id]...)
	}
	return out, nil
}

func (f *epFakeEdges) OutboundEdges(_ context.Context, _, _ string, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

func TestNewEntryPointsService_RejectsNilDependencies(t *testing.T) {
	blast := blastradius.NewService(&epFakeEdges{}, &epFakeNodes{}, nil)
	lg := func(context.Context, string, string) (*domain.Graph, error) { return nil, nil }
	ie := func(context.Context, string, string, []string) (map[string][]string, error) { return nil, nil }
	of := func(context.Context, string, string) (map[string]bool, error) { return nil, nil }

	if _, err := NewEntryPointsService(nil, ie, of, blast); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil loadGraph: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewEntryPointsService(lg, nil, of, blast); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil inboundEdges: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewEntryPointsService(lg, ie, nil, blast); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil openFindings: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewEntryPointsService(lg, ie, of, nil); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil blast: want ErrMissingDependency, got %v", err)
	}
}

// AC1: candidates are symbols with low blast radius, an adjacent test, and
// no open findings.
func TestSelect_AppliesAllThreeCandidateGates(t *testing.T) {
	svc := epFixtureService(t)
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(rep.EntryPoints) != 1 {
		t.Fatalf("expected exactly 1 entry point, got %d (%+v)", len(rep.EntryPoints), rep.EntryPoints)
	}
	ep := rep.EntryPoints[0]
	if ep.SymbolName != "low" {
		t.Errorf("expected 'low' to be the only candidate, got %q", ep.SymbolName)
	}
	if ep.BlastRadius != 2 {
		t.Errorf("low: expected blast radius 2 (seed + low_test), got %d", ep.BlastRadius)
	}
	if ep.FilePath != "app/low.go" {
		t.Errorf("low: expected file app/low.go, got %q", ep.FilePath)
	}
}

// AC1: the low-blast-radius threshold is configurable.
func TestSelect_MaxBlastRadiusIsConfigurable(t *testing.T) {
	// Raise the cap so "heavy" (radius 12: seed + 11 callers) also qualifies.
	svc := epFixtureService(t, WithMaxBlastRadius(20))
	rep, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(rep.EntryPoints) != 2 {
		t.Fatalf("WithMaxBlastRadius(5): expected 2 entry points, got %d (%+v)", len(rep.EntryPoints), rep.EntryPoints)
	}
	// Deterministic order: ascending blast radius, then symbol name.
	if rep.EntryPoints[0].SymbolName != "low" || rep.EntryPoints[1].SymbolName != "heavy" {
		t.Errorf("expected [low, heavy], got %+v", rep.EntryPoints)
	}
}

// AC2: rendering the same promoted state twice yields byte-identical output.
func TestRenderEntryPoints_Deterministic(t *testing.T) {
	svc := epFixtureService(t, WithMaxBlastRadius(20))
	rep1, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	rep2, err := svc.Select(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	out1 := RenderEntryPoints(rep1)
	out2 := RenderEntryPoints(rep2)
	if out1 != out2 {
		t.Errorf("render not byte-identical:\n--- 1 ---\n%s\n--- 2 ---\n%s", out1, out2)
	}
	if !strings.Contains(out1, "low") || !strings.Contains(out1, "| low |") {
		t.Errorf("rendered page missing expected row:\n%s", out1)
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
