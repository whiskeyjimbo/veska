package blastradius_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeEdges is a deterministic in-memory ports.EdgeReader.
type fakeEdges struct {
	inbound  map[string][]string
	outbound map[string][]string
	err      error
}

func (f *fakeEdges) InboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.inbound[id]...)
	}
	return out, nil
}

func (f *fakeEdges) OutboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.outbound[id]...)
	}
	return out, nil
}

type fakeNodes struct {
	metas     map[string]ports.NodeMeta
	byFile    map[string][]string
	inFileErr error
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
	if f.inFileErr != nil {
		return nil, f.inFileErr
	}
	return f.byFile[filePath], nil
}

func TestOf_DefaultDirectionWalksInbound(t *testing.T) {
	edges := &fakeEdges{
		inbound: map[string][]string{
			"seed": {"a", "b"},
			"a":    {"c"},
		},
	}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"seed": {NodeID: "seed", SymbolPath: "s"},
		"a":    {NodeID: "a", SymbolPath: "A"},
		"b":    {NodeID: "b", SymbolPath: "B"},
		"c":    {NodeID: "c", SymbolPath: "C"},
	}}
	s := blastradius.NewService(edges, nodes, nil)
	resp, err := s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{MaxDepth: 2})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	wantIDs := []string{"seed", "a", "b", "c"}
	wantDist := map[string]int{"seed": 0, "a": 1, "b": 1, "c": 2}
	if len(resp.Entries) != len(wantIDs) {
		t.Fatalf("got %d entries, want %d (%+v)", len(resp.Entries), len(wantIDs), resp.Entries)
	}
	for i, e := range resp.Entries {
		if e.NodeID != wantIDs[i] {
			t.Errorf("entry %d: got %q, want %q", i, e.NodeID, wantIDs[i])
		}
		if e.Distance != wantDist[e.NodeID] {
			t.Errorf("entry %q: distance %d, want %d", e.NodeID, e.Distance, wantDist[e.NodeID])
		}
	}
}

func TestOf_OutboundDirection(t *testing.T) {
	edges := &fakeEdges{
		outbound: map[string][]string{
			"seed": {"a"},
			"a":    {"b"},
		},
	}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"seed": {NodeID: "seed"},
		"a":    {NodeID: "a"},
		"b":    {NodeID: "b"},
	}}
	s := blastradius.NewService(edges, nodes, nil)
	resp, err := s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{
		MaxDepth: 5, Direction: blastradius.DirCallees,
	})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	got := make([]string, len(resp.Entries))
	for i, e := range resp.Entries {
		got[i] = e.NodeID
	}
	want := []string{"seed", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pos %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestOf_BothDirections(t *testing.T) {
	edges := &fakeEdges{
		inbound:  map[string][]string{"seed": {"caller"}},
		outbound: map[string][]string{"seed": {"callee"}},
	}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"seed": {NodeID: "seed"}, "caller": {NodeID: "caller"}, "callee": {NodeID: "callee"},
	}}
	s := blastradius.NewService(edges, nodes, nil)
	resp, err := s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{
		MaxDepth: 1, Direction: blastradius.DirBoth,
	})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	ids := make(map[string]bool)
	for _, e := range resp.Entries {
		ids[e.NodeID] = true
	}
	for _, want := range []string{"seed", "caller", "callee"} {
		if !ids[want] {
			t.Errorf("expected %s in radius, got %+v", want, resp.Entries)
		}
	}
}

func TestOf_TruncatedAtMaxNodes(t *testing.T) {
	edges := &fakeEdges{inbound: map[string][]string{
		"seed": {"a", "b", "c", "d", "e"},
	}}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{}}
	s := blastradius.NewService(edges, nodes, nil)
	resp, err := s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{
		MaxDepth: 2, MaxNodes: 3,
	})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if !resp.Truncated {
		t.Error("expected Truncated=true")
	}
	if len(resp.Entries) != 3 {
		t.Fatalf("expected exactly MaxNodes=3 entries, got %d", len(resp.Entries))
	}
}

func TestOf_PropagatesEdgeError(t *testing.T) {
	edges := &fakeEdges{err: errors.New("db down")}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{}}
	s := blastradius.NewService(edges, nodes, nil)
	_, err := s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{MaxDepth: 1})
	if err == nil {
		t.Fatal("expected error, got nil")
		return
	}
}

func TestOf_DeduplicatesSeeds(t *testing.T) {
	edges := &fakeEdges{}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{"s": {NodeID: "s"}}}
	s := blastradius.NewService(edges, nodes, nil)
	resp, err := s.Of(context.Background(), "r", "main", []string{"s", "s", "s"}, blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected 1 dedup'd seed, got %d", len(resp.Entries))
	}
}

func TestDirtyOf_UsesStagedNodes(t *testing.T) {
	staging := application.NewStagingArea()
	n, err := domain.NewNode("staged-1", "foo.go", "Foo", domain.KindFunction)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	staging.StageFile("r", "main", "foo.go", []*domain.Node{n}, nil)

	edges := &fakeEdges{inbound: map[string][]string{
		"staged-1": {"caller-a"},
	}}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"staged-1": {NodeID: "staged-1"},
		"caller-a": {NodeID: "caller-a"},
	}}
	s := blastradius.NewService(edges, nodes, staging)
	resp, err := s.DirtyOf(context.Background(), "r", "main", blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DirtyOf: %v", err)
	}
	if !resp.IncludedStaging {
		t.Error("expected IncludedStaging=true")
	}
	ids := make([]string, len(resp.Entries))
	for i, e := range resp.Entries {
		ids[i] = e.NodeID
	}
	wantHas := func(id string) bool {
		return slices.Contains(ids, id)
	}
	if !wantHas("staged-1") || !wantHas("caller-a") {
		t.Errorf("expected staged-1 and caller-a, got %v", ids)
	}
}

func TestDirtyOf_NilStagingReturnsEmpty(t *testing.T) {
	edges := &fakeEdges{}
	nodes := &fakeNodes{}
	s := blastradius.NewService(edges, nodes, nil)
	resp, err := s.DirtyOf(context.Background(), "r", "main", blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DirtyOf: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("expected empty, got %+v", resp.Entries)
	}
	if !resp.IncludedStaging {
		t.Error("IncludedStaging should still be true for the dirty path")
	}
}

func TestDiffOf_UnionAcrossChangedFiles(t *testing.T) {
	edges := &fakeEdges{inbound: map[string][]string{
		"a": {"caller-of-a"},
	}}
	nodes := &fakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "b": {NodeID: "b"}, "caller-of-a": {NodeID: "caller-of-a"},
		},
		byFile: map[string][]string{
			"foo.go": {"a"},
			"bar.go": {"b"},
		},
	}
	s := blastradius.NewService(edges, nodes, nil)
	changed := func(_ context.Context, _ string) ([]string, error) {
		return []string{"foo.go", "bar.go"}, nil
	}
	resp, err := s.DiffOf(context.Background(), "r", "main", "/tmp/repo", changed, blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DiffOf: %v", err)
	}
	got := make(map[string]bool)
	for _, e := range resp.Entries {
		got[e.NodeID] = true
	}
	for _, id := range []string{"a", "b", "caller-of-a"} {
		if !got[id] {
			t.Errorf("expected %s in entries, got %+v", id, resp.Entries)
		}
	}
}

func TestDiffOf_EmptyDiffEmptyResponse(t *testing.T) {
	s := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	resp, err := s.DiffOf(context.Background(), "r", "main", "/tmp/r", func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}, blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DiffOf: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("expected no entries, got %+v", resp.Entries)
	}
}

func TestDiffOf_FilesWithNoNodesShortCircuits(t *testing.T) {
	s := blastradius.NewService(&fakeEdges{}, &fakeNodes{byFile: map[string][]string{}}, nil)
	resp, err := s.DiffOf(context.Background(), "r", "main", "/tmp/r", func(_ context.Context, _ string) ([]string, error) {
		return []string{"new.go", "vendor.go"}, nil
	}, blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DiffOf: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("expected empty (no promoted nodes), got %+v", resp.Entries)
	}
}

func TestDiffOf_RejectsNilChangedFilesFunc(t *testing.T) {
	s := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	_, err := s.DiffOf(context.Background(), "r", "main", "/tmp/r", nil, blastradius.Options{})
	if err == nil {
		t.Error("expected error for nil changedFiles")
	}
}

func TestDiffOf_RejectsEmptyRepoRoot(t *testing.T) {
	s := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	_, err := s.DiffOf(context.Background(), "r", "main", "", func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}, blastradius.Options{})
	if err == nil {
		t.Error("expected error for empty repoRoot")
	}
}

func TestParseDirection(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want blastradius.Direction
		err  bool
	}{
		{"", blastradius.DirCallers, false},
		{"callers", blastradius.DirCallers, false},
		{"callees", blastradius.DirCallees, false},
		{"both", blastradius.DirBoth, false},
		{"sideways", "", true},
	} {
		got, err := blastradius.ParseDirection(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseDirection(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDirection(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseDirection(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}
