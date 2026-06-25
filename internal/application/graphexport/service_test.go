// SPDX-License-Identifier: AGPL-3.0-only

package graphexport

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

const testRepoRoot = "/home/alice/src/proj"

// fixtureGraph builds a small graph that exercises every projection branch:
// a first-party node with a stored summary, a first-party node that must fall
// back to the heuristic summary, an External node that must be dropped, a
// resolved edge that must survive, and an unresolved (proposed) edge that must
// be dropped.
func fixtureGraph(t *testing.T) *domain.Graph {
	t.Helper()
	g, err := domain.NewGraph("repo1", "main")
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	mustAddNode(t, g, domain.NodeSpec{ID: "n-bravo", Path: testRepoRoot + "/pkg/b.go", Name: "Bravo", Kind: domain.KindFunction},
		domain.WithLines(domain.LineRange{Start: 10, End: 20}),
		domain.WithExported(true),
		domain.WithLanguage("go"),
		domain.WithRawContent("func Bravo() {}"),
		domain.WithShortSummary("does the bravo thing"),
	)
	// no stored summary + no signature -> heuristic "function Alpha"
	mustAddNode(t, g, domain.NodeSpec{ID: "n-alpha", Path: testRepoRoot + "/pkg/a.go", Name: "Alpha", Kind: domain.KindFunction})
	// External node must be excluded from nodes[]
	mustAddNode(t, g, domain.NodeSpec{ID: "n-ext", Path: "/go/pkg/mod/x/y.go", Name: "Vendored", Kind: domain.KindFunction},
		domain.WithExternal(true),
	)
	mustAddEdge(t, g, domain.EdgeSpec{Src: "n-alpha", Tgt: "n-bravo", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Definite))
	// unresolved proposed similarity edge must be excluded from edges[]
	mustAddEdge(t, g, domain.EdgeSpec{Src: "n-bravo", Tgt: "n-alpha", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Unresolved))
	// resolved edge into the excluded External node must be dropped too, so no
	// edge dangles to a node absent from nodes[].
	mustAddEdge(t, g, domain.EdgeSpec{Src: "n-bravo", Tgt: "n-ext", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Definite))
	return g
}

func mustAddNode(t *testing.T, g *domain.Graph, spec domain.NodeSpec, opts ...domain.NodeOption) {
	t.Helper()
	n, err := domain.NewNode(spec, opts...)
	if err != nil {
		t.Fatalf("NewNode %s: %v", spec.ID, err)
	}
	if err := g.AddNode(n); err != nil {
		t.Fatalf("AddNode %s: %v", spec.ID, err)
	}
}

func mustAddEdge(t *testing.T, g *domain.Graph, spec domain.EdgeSpec, opts ...domain.EdgeOption) {
	t.Helper()
	e, err := domain.NewEdge(spec, opts...)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	if err := g.AddEdge(e); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
}

func newFixtureService(t *testing.T, g *domain.Graph) *Service {
	t.Helper()
	svc, err := NewService(
		func(context.Context, string, string) (*domain.Graph, error) { return g, nil },
		func(context.Context, string, string, string) (wiki.Report, error) {
			return wiki.Report{Zones: []wiki.HotZone{
				{FilePath: testRepoRoot + "/pkg/b.go", RecentChangeFrequency: 3, BlastRadius: 5, Score: 15},
			}}, nil
		},
		func(context.Context, string, string) (wiki.EntryPointsReport, error) {
			return wiki.EntryPointsReport{EntryPoints: []wiki.EntryPoint{
				{SymbolName: "Bravo", FilePath: testRepoRoot + "/pkg/b.go", Kind: "function", InboundCount: 2, Exported: true, HasAdjacentTest: true},
			}}, nil
		},
		func(context.Context, string, string) (dependencies.Result, error) {
			return dependencies.Result{Dependencies: []dependencies.Dependency{
				{Module: "golang.org/x/text", Version: "v0.14.0", Language: "go", UsageCount: 4, TopCallSites: []dependencies.CallSite{{SrcNodeID: "n-bravo", SymbolPath: "x/text.Foo"}}},
			}}, nil
		},
		func(context.Context, string, string) (map[string]string, error) {
			// alpha's source comes from the snippet map (no node RawContent);
			// bravo's node RawContent wins over any map entry.
			return map[string]string{"n-alpha": "func Alpha() {}"}, nil
		},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// exportFixture runs the fixture export once and fails the test on error.
func exportFixture(t *testing.T) Snapshot {
	t.Helper()
	svc := newFixtureService(t, fixtureGraph(t))
	snap, err := svc.Export(context.Background(), "repo1", "main", testRepoRoot)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return snap
}

func TestExportNodeProjection(t *testing.T) {
	snap := exportFixture(t)

	if snap.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", snap.SchemaVersion, SchemaVersion)
	}
	// External node dropped; first-party nodes kept and id-sorted.
	if len(snap.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (external excluded)", len(snap.Nodes))
	}
	if snap.Nodes[0].ID != "n-alpha" || snap.Nodes[1].ID != "n-bravo" {
		t.Errorf("nodes not id-sorted: %s, %s", snap.Nodes[0].ID, snap.Nodes[1].ID)
	}
	// Summary fallback: alpha has no stored summary -> heuristic; bravo stored.
	if got := snap.Nodes[0].Summary; got != "function Alpha" {
		t.Errorf("alpha summary = %q, want heuristic", got)
	}
	if got := snap.Nodes[1].Summary; got != "does the bravo thing" {
		t.Errorf("bravo summary = %q, want stored", got)
	}
	// raw_content: bravo from node RawContent, alpha from the snippet map.
	if got := snap.Nodes[1].RawContent; got != "func Bravo() {}" {
		t.Errorf("bravo raw_content = %q (want node RawContent)", got)
	}
	if got := snap.Nodes[0].RawContent; got != "func Alpha() {}" {
		t.Errorf("alpha raw_content = %q (want snippet-map source)", got)
	}
}

func TestExportEdgeProjection(t *testing.T) {
	snap := exportFixture(t)
	// Only the resolved alpha->bravo CALLS edge survives: the unresolved
	// SIMILAR_TO edge and the resolved edge into the excluded External node
	// are both dropped.
	if len(snap.Edges) != 1 {
		t.Fatalf("got %d edges, want 1 (unresolved + dangling excluded): %+v", len(snap.Edges), snap.Edges)
	}
	if snap.Edges[0].Kind != "CALLS" || snap.Edges[0].Confidence != "definite" {
		t.Errorf("edge = %+v, want CALLS/definite", snap.Edges[0])
	}
	// Self-consistency: every emitted edge endpoint resolves to a node.
	ids := make(map[string]struct{}, len(snap.Nodes))
	for _, n := range snap.Nodes {
		ids[n.ID] = struct{}{}
	}
	for _, e := range snap.Edges {
		if _, ok := ids[e.Src]; !ok {
			t.Errorf("edge %s has dangling src %s", e.ID, e.Src)
		}
		if _, ok := ids[e.Tgt]; !ok {
			t.Errorf("edge %s has dangling tgt %s", e.ID, e.Tgt)
		}
	}
}

// TestExportRelativizesPaths is the AC2 gate: every emitted path is
// repo-root-relative and no absolute home path survives into the bytes.
func TestExportRelativizesPaths(t *testing.T) {
	snap := exportFixture(t)
	for _, n := range snap.Nodes {
		if strings.HasPrefix(n.Path, "/") {
			t.Errorf("node path not relativized: %q", n.Path)
		}
	}
	if got := snap.Nodes[1].Path; got != "pkg/b.go" {
		t.Errorf("bravo path = %q, want pkg/b.go", got)
	}
	if got := snap.HotZones[0].FilePath; got != "pkg/b.go" {
		t.Errorf("hot zone path = %q", got)
	}
	if got := snap.EntryPoints[0].FilePath; got != "pkg/b.go" {
		t.Errorf("entry point path = %q", got)
	}
	b, err := Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "/home/alice") {
		t.Errorf("snapshot leaks absolute home path:\n%s", b)
	}
}

func TestExportReusesDependencies(t *testing.T) {
	snap := exportFixture(t)
	if len(snap.Dependencies) != 1 || snap.Dependencies[0].Module != "golang.org/x/text" {
		t.Errorf("dependencies = %+v", snap.Dependencies)
	}
}

// TestExportByteDeterministic is the AC3 gate: re-exporting an unchanged graph
// yields byte-identical JSON. Iteration order over the graph's internal maps
// must not leak into the output.
func TestExportByteDeterministic(t *testing.T) {
	for range 8 {
		// Rebuild the graph each iteration so any map-ordering nondeterminism
		// in construction has a fresh chance to surface.
		svc := newFixtureService(t, fixtureGraph(t))
		first, err := svc.Export(context.Background(), "repo1", "main", testRepoRoot)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		second, err := svc.Export(context.Background(), "repo1", "main", testRepoRoot)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		a, err := Marshal(first)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		b, err := Marshal(second)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("snapshot not byte-identical across runs:\n--- a ---\n%s\n--- b ---\n%s", a, b)
		}
	}
}

func TestNewServiceRejectsNilDeps(t *testing.T) {
	ok := func(context.Context, string, string) (*domain.Graph, error) { return nil, nil }
	if _, err := NewService(nil, nil, nil, nil, nil); err == nil {
		t.Fatal("expected error on nil loadGraph")
	}
	if _, err := NewService(ok, nil, nil, nil, nil); err == nil {
		t.Fatal("expected error on nil rankHotZones")
	}
}
