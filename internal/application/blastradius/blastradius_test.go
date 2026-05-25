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
	// Seed must resolve, even if downstream nodes don't — solov2-2w0u.
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{"seed": {NodeID: "seed"}}}
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
	// Seed metadata must be present so the new ErrSeedNotFound gate doesn't
	// short-circuit before we reach the edge query (solov2-2w0u).
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{"seed": {NodeID: "seed"}}}
	s := blastradius.NewService(edges, nodes, nil)
	_, err := s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{MaxDepth: 1})
	if err == nil {
		t.Fatal("expected error, got nil")
		return
	}
	if errors.Is(err, blastradius.ErrSeedNotFound) {
		t.Fatalf("got ErrSeedNotFound, wanted the edge-store error to propagate: %v", err)
	}
}

func TestOf_SeedNotFound_ReturnsErrSeedNotFound(t *testing.T) {
	// Regression for solov2-2w0u: when the supplied seed_id doesn't resolve
	// in (repoID, branch), Of must return ErrSeedNotFound instead of an
	// empty-fields entry that masked the real cause for MCP callers.
	edges := &fakeEdges{}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{}}
	s := blastradius.NewService(edges, nodes, nil)
	_, err := s.Of(context.Background(), "r", "main", []string{"deadbeef"}, blastradius.Options{MaxDepth: 1})
	if err == nil {
		t.Fatal("want error for unknown seed, got nil")
	}
	if !errors.Is(err, blastradius.ErrSeedNotFound) {
		t.Fatalf("want ErrSeedNotFound, got %v", err)
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

// TestOf_HubDegreeThresholdSuppressesFanout guards solov2-l2f5: when a node
// in the BFS frontier has more neighbours than HubDegreeThreshold, the
// walker does NOT expand through it. The node itself still appears in the
// result with IsHub=true so callers see the structural fact; what's
// excluded is the irrelevant fan-out. This models cobra's rootCmd
// (every command's init adds to it → 100+ inbound edges).
func TestOf_HubDegreeThresholdSuppressesFanout(t *testing.T) {
	// hub has 6 inbound edges. cmd-a (a leaf with 1 inbound) reaches hub
	// via init-a. Without gating, the rest of the cmd-* siblings would be
	// pulled in at distance 2.
	edges := &fakeEdges{inbound: map[string][]string{
		"hub":    {"init-a", "init-b", "init-c", "init-d", "init-e", "init-f"},
		"init-a": {"cmd-a"},
		"init-b": {"cmd-b"},
		"init-c": {"cmd-c"},
		"init-d": {"cmd-d"},
		"init-e": {"cmd-e"},
		"init-f": {"cmd-f"},
		"cmd-a":  {"hub"}, // seed reaches hub via 1-hop inbound
	}}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"cmd-a": {NodeID: "cmd-a"}, "hub": {NodeID: "hub"},
		"init-a": {NodeID: "init-a"}, "init-b": {NodeID: "init-b"},
		"init-c": {NodeID: "init-c"}, "init-d": {NodeID: "init-d"},
		"init-e": {NodeID: "init-e"}, "init-f": {NodeID: "init-f"},
	}}
	s := blastradius.NewService(edges, nodes, nil)

	// With gating (threshold 3): hub appears as IsHub=true; init-b..f are
	// NOT in the result because BFS didn't expand through hub.
	gated, err := s.Of(context.Background(), "r", "main", []string{"cmd-a"},
		blastradius.Options{MaxDepth: 3, HubDegreeThreshold: 3})
	if err != nil {
		t.Fatalf("Of(gated): %v", err)
	}
	ids := map[string]bool{}
	hubMarked := false
	for _, e := range gated.Entries {
		ids[e.NodeID] = true
		if e.NodeID == "hub" && e.IsHub {
			hubMarked = true
		}
	}
	if !ids["cmd-a"] || !ids["hub"] {
		t.Errorf("expected cmd-a and hub in gated result, got %+v", ids)
	}
	if !hubMarked {
		t.Errorf("expected hub entry to carry IsHub=true")
	}
	for _, sib := range []string{"init-b", "init-c", "init-d", "init-e", "init-f"} {
		if ids[sib] {
			t.Errorf("gated BFS expanded through hub: %s should be absent", sib)
		}
	}

	// With gating disabled (threshold -1): legacy behaviour — every sibling
	// init-* is pulled in at distance 3 (cmd-a → hub → init-b → ...).
	wide, err := s.Of(context.Background(), "r", "main", []string{"cmd-a"},
		blastradius.Options{MaxDepth: 3, HubDegreeThreshold: -1})
	if err != nil {
		t.Fatalf("Of(wide): %v", err)
	}
	wideIDs := map[string]bool{}
	for _, e := range wide.Entries {
		wideIDs[e.NodeID] = true
	}
	for _, sib := range []string{"init-b", "init-c", "init-d", "init-e", "init-f"} {
		if !wideIDs[sib] {
			t.Errorf("ungated BFS should reach %s via hub; missing", sib)
		}
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
