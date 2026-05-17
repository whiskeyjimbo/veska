package wiki

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeEdges is a deterministic EdgeReader fixture.
type fakeEdges struct {
	inbound map[string][]string
}

func (f *fakeEdges) InboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.inbound[id]...)
	}
	return out, nil
}

func (f *fakeEdges) OutboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// fakeNodes is a deterministic NodeLookup fixture.
type fakeNodes struct {
	metas  map[string]ports.NodeMeta
	byFile map[string][]string
}

func (f *fakeNodes) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	var out []ports.NodeMeta
	for _, id := range ids {
		if m, ok := f.metas[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeNodes) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	return f.byFile[filePath], nil
}

// fixtureService builds a HotZoneService over a fixed promoted state:
//   - a.go: 5 commits, 1 node "a", 2 inbound callers -> radius 3, score 15
//   - b.go: 5 commits, 1 node "b", 0 callers          -> radius 1, score 5
//   - c.go: 3 commits, 1 node "c", 2 inbound callers  -> radius 3, score 9
func fixtureService(t *testing.T, opts ...Option) *HotZoneService {
	t.Helper()
	edges := &fakeEdges{inbound: map[string][]string{
		"a": {"x", "y"},
		"c": {"x", "y"},
	}}
	nodes := &fakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "b": {NodeID: "b"}, "c": {NodeID: "c"},
			"x": {NodeID: "x"}, "y": {NodeID: "y"},
		},
		byFile: map[string][]string{
			"a.go": {"a"}, "b.go": {"b"}, "c.go": {"c"},
		},
	}
	blast := blastradius.NewService(edges, nodes, nil)
	counts := func(_ context.Context, _ string) (map[string]int, error) {
		return map[string]int{"a.go": 5, "b.go": 5, "c.go": 3}, nil
	}
	svc, err := NewHotZoneService(counts, nodes.NodesInFile, blast, opts...)
	if err != nil {
		t.Fatalf("NewHotZoneService: %v", err)
	}
	return svc
}

func TestNewHotZoneService_RejectsNilDependencies(t *testing.T) {
	blast := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	counts := func(context.Context, string) (map[string]int, error) { return nil, nil }
	nif := func(context.Context, string, string, string) ([]string, error) { return nil, nil }

	if _, err := NewHotZoneService(nil, nif, blast); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil changeCounts: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHotZoneService(counts, nil, blast); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil nodesInFile: want ErrMissingDependency, got %v", err)
	}
	if _, err := NewHotZoneService(counts, nif, nil); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("nil blast: want ErrMissingDependency, got %v", err)
	}
}

// AC1: files ranked by recent_change_frequency × blast_radius.
func TestRank_RanksByFrequencyTimesBlastRadius(t *testing.T) {
	svc := fixtureService(t)
	rep, err := svc.Rank(context.Background(), "r1", "main", "/tmp/r")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(rep.Zones) != 3 {
		t.Fatalf("expected 3 zones, got %d", len(rep.Zones))
	}
	want := []struct {
		path  string
		score int
	}{
		{"a.go", 15},
		{"c.go", 9},
		{"b.go", 5},
	}
	for i, w := range want {
		if rep.Zones[i].FilePath != w.path || rep.Zones[i].Score != w.score {
			t.Errorf("zone %d: got %+v, want path=%s score=%d", i, rep.Zones[i], w.path, w.score)
		}
	}
}

// AC1: top-N is configurable.
func TestRank_TopNIsConfigurable(t *testing.T) {
	svc := fixtureService(t, WithTopN(2))
	rep, err := svc.Rank(context.Background(), "r1", "main", "/tmp/r")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if len(rep.Zones) != 2 {
		t.Fatalf("WithTopN(2): expected 2 zones, got %d", len(rep.Zones))
	}
	if rep.Zones[0].FilePath != "a.go" || rep.Zones[1].FilePath != "c.go" {
		t.Errorf("expected top-2 a.go,c.go, got %+v", rep.Zones)
	}
}

// AC2: rendering the same promoted state twice yields byte-identical output.
func TestRenderHotZones_Deterministic(t *testing.T) {
	svc := fixtureService(t)
	rep1, err := svc.Rank(context.Background(), "r1", "main", "/tmp/r")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	rep2, err := svc.Rank(context.Background(), "r1", "main", "/tmp/r")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	out1 := RenderHotZones(rep1)
	out2 := RenderHotZones(rep2)
	if out1 != out2 {
		t.Errorf("render not byte-identical:\n--- 1 ---\n%s\n--- 2 ---\n%s", out1, out2)
	}
	if !contains(out1, "a.go") || !contains(out1, "| 1 | a.go |") {
		t.Errorf("rendered page missing expected top row:\n%s", out1)
	}
}

func TestRenderHotZones_EmptyReport(t *testing.T) {
	out := RenderHotZones(Report{RepoID: "r1", Branch: "main"})
	if !contains(out, "No hot zones") {
		t.Errorf("empty report should render a placeholder, got:\n%s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
