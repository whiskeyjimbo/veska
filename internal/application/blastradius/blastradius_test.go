// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package blastradius_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
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
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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
	// Seed must resolve, even if downstream nodes don't.
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{"seed": {NodeID: "seed"}}}
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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
	// short-circuit before we reach the edge query.
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{"seed": {NodeID: "seed"}}}
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = s.Of(context.Background(), "r", "main", []string{"seed"}, blastradius.Options{MaxDepth: 1})
	if err == nil {
		t.Fatal("expected error, got nil")
		return
	}
	if errors.Is(err, blastradius.ErrSeedNotFound) {
		t.Fatalf("got ErrSeedNotFound, wanted the edge-store error to propagate: %v", err)
	}
}

func TestOf_SeedNotFound_ReturnsErrSeedNotFound(t *testing.T) {
	// Regression for: when the supplied seed_id doesn't resolve
	// in (repoID, branch), Of must return ErrSeedNotFound instead of an
	// empty-fields entry that masked the real cause for MCP callers.
	edges := &fakeEdges{}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{}}
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = s.Of(context.Background(), "r", "main", []string{"deadbeef"}, blastradius.Options{MaxDepth: 1})
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
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	resp, err := s.Of(context.Background(), "r", "main", []string{"s", "s", "s"}, blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected 1 dedup'd seed, got %d", len(resp.Entries))
	}
}

func TestDirtyOf_UsesStagedNodes(t *testing.T) {
	area := staging.NewArea()
	n, err := domain.NewNode(domain.NodeSpec{ID: "staged-1", Path: "foo.go", Name: "Foo", Kind: domain.KindFunction})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	area.Stage("r", "main", "foo.go", staging.File{Nodes: []*domain.Node{n}, Edges: nil})

	edges := &fakeEdges{inbound: map[string][]string{
		"staged-1": {"caller-a"},
	}}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"staged-1": {NodeID: "staged-1"},
		"caller-a": {NodeID: "caller-a"},
	}}
	s, err := blastradius.NewService(edges, nodes, area)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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

// fakeNodesWithHashes adds ContentHasher to fakeNodes for the
// unchanged-symbol filter test.
type fakeNodesWithHashes struct {
	*fakeNodes
	hashes map[string]string
}

func (f *fakeNodesWithHashes) NodeContentHash(_ context.Context, _, _, nodeID string) (string, error) {
	return f.hashes[nodeID], nil
}

// TestDirtyOf_SkipsUnchangedSymbols covers: a re-parse stages
// every symbol in a touched file, but a node whose ContentHash matches the
// promoted hash isn't actually dirty (e.g. comment-only edit). DirtyOf
// must filter such nodes out of the seed set so the response doesn't
// dirty the whole file.
func TestDirtyOf_SkipsUnchangedSymbols(t *testing.T) {
	area := staging.NewArea()
	// Build nodes with explicit ContentHashes: one matches the promoted
	// hash ("unchanged"), one differs ("changed").
	unchanged, err := domain.NewNode(domain.NodeSpec{ID: "unchanged", Path: "foo.go", Name: "Same", Kind: domain.KindFunction}, domain.WithContentHash("HASH-A"))
	if err != nil {
		t.Fatalf("NewNode unchanged: %v", err)
	}
	changed, err := domain.NewNode(domain.NodeSpec{ID: "changed", Path: "foo.go", Name: "Edited", Kind: domain.KindFunction}, domain.WithContentHash("HASH-B-NEW"))
	if err != nil {
		t.Fatalf("NewNode changed: %v", err)
	}
	area.Stage("r", "main", "foo.go", staging.File{Nodes: []*domain.Node{unchanged, changed}, Edges: nil})

	edges := &fakeEdges{inbound: map[string][]string{
		"unchanged": {"caller-of-same"},
		"changed":   {"caller-of-edited"},
	}}
	nodes := &fakeNodesWithHashes{
		fakeNodes: &fakeNodes{metas: map[string]ports.NodeMeta{
			"unchanged":        {NodeID: "unchanged"},
			"changed":          {NodeID: "changed"},
			"caller-of-same":   {NodeID: "caller-of-same"},
			"caller-of-edited": {NodeID: "caller-of-edited"},
		}},
		hashes: map[string]string{
			"unchanged": "HASH-A",     // matches staged → must be skipped
			"changed":   "HASH-B-OLD", // differs from staged HASH-B-NEW → seeded
		},
	}
	s, err := blastradius.NewService(edges, nodes, area)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	resp, err := s.DirtyOf(context.Background(), "r", "main", blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DirtyOf: %v", err)
	}
	ids := make([]string, len(resp.Entries))
	for i, e := range resp.Entries {
		ids[i] = e.NodeID
	}
	if slices.Contains(ids, "unchanged") {
		t.Errorf("unchanged symbol must be filtered out of dirty seeds; got %v", ids)
	}
	if !slices.Contains(ids, "changed") {
		t.Errorf("changed symbol must remain a seed; got %v", ids)
	}
	if slices.Contains(ids, "caller-of-same") {
		t.Errorf("BFS must not expand from unchanged; got %v", ids)
	}
	if !slices.Contains(ids, "caller-of-edited") {
		t.Errorf("BFS must expand from changed; got %v", ids)
	}
}

func TestDirtyOf_NilStagingReturnsEmpty(t *testing.T) {
	edges := &fakeEdges{}
	nodes := &fakeNodes{}
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	resp, err := s.DirtyOf(context.Background(), "r", "main", blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DirtyOf: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("expected empty, got %+v", resp.Entries)
	}
	// No staging area → staging contributed nothing.
	if resp.IncludedStaging {
		t.Error("IncludedStaging must be false when there is no staging area")
	}
}

// TestDirtyOf_CleanTreeReportsNoStaging is the regression: with a
// staging area present but no dirty nodes, DirtyOf contributes no seeds and must
// report IncludedStaging=false - the flag means "staging contributed rows"
// ( 4.4), not merely "this is the dirty view".
func TestDirtyOf_CleanTreeReportsNoStaging(t *testing.T) {
	area := staging.NewArea() // present but empty: nothing staged
	s, err := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, area)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	resp, err := s.DirtyOf(context.Background(), "r", "main", blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DirtyOf: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("expected empty entries for a clean tree, got %+v", resp.Entries)
	}
	if resp.IncludedStaging {
		t.Error("IncludedStaging must be false when nothing is staged")
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
		// nodes are keyed by the repo-relative slash path, which
		// is the form git diff yields, so DiffOf feeds them through directly.
		byFile: map[string][]string{
			"foo.go": {"a"},
			"bar.go": {"b"},
		},
	}
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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

// TestDiffOf_MatchesRelativeDiffPaths pins: git diff yields
// repo-relative paths and nodes.file_path is now stored repo-relative too, so a
// diff path feeds NodesInFile directly without an absolutize step.
func TestDiffOf_MatchesRelativeDiffPaths(t *testing.T) {
	nodes := &fakeNodes{
		metas:  map[string]ports.NodeMeta{"a": {NodeID: "a"}},
		byFile: map[string][]string{"flag.go": {"a"}},
	}
	s, err := blastradius.NewService(&fakeEdges{}, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	changed := func(_ context.Context, _ string) ([]string, error) {
		return []string{"flag.go"}, nil // repo-relative, as git emits
	}
	resp, err := s.DiffOf(context.Background(), "r", "main", "/tmp/junior-pflag", changed, blastradius.Options{MaxDepth: 1})
	if err != nil {
		t.Fatalf("DiffOf: %v", err)
	}
	if len(resp.Entries) == 0 {
		t.Fatal("expected the relative-stored node to be found from a relative diff path, got empty")
	}
}

func TestDiffOf_EmptyDiffEmptyResponse(t *testing.T) {
	s, err := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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
	s, err := blastradius.NewService(&fakeEdges{}, &fakeNodes{byFile: map[string][]string{}}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
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
	s, err := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = s.DiffOf(context.Background(), "r", "main", "/tmp/r", nil, blastradius.Options{})
	if err == nil {
		t.Error("expected error for nil changedFiles")
	}
}

func TestDiffOf_RejectsEmptyRepoRoot(t *testing.T) {
	s, err := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = s.DiffOf(context.Background(), "r", "main", "", func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}, blastradius.Options{})
	if err == nil {
		t.Error("expected error for empty repoRoot")
	}
}

// TestOf_HubDegreeThresholdSuppressesFanout guards: when a node
// in the BFS frontier has more neighbors than HubDegreeThreshold, the
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
	s, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	// With gating (threshold 3): hub appears as IsHub=true; init-b.f are
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

	// With gating disabled (threshold -1): legacy behavior - every sibling
	// init-* is pulled in at distance 3 (cmd-a → hub → init-b →.).
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

// hubFixture builds the star-shaped graph used by the configured-default tests:
// cmd-a reaches a 6-degree hub one hop in, and each init-* sibling hangs off
// the hub. Whether the init-b.f siblings appear depends on the hub gate.
func hubFixture() (*fakeEdges, *fakeNodes) {
	edges := &fakeEdges{inbound: map[string][]string{
		"hub":    {"init-a", "init-b", "init-c", "init-d", "init-e", "init-f"},
		"init-a": {"cmd-a"},
		"init-b": {"cmd-b"},
		"init-c": {"cmd-c"},
		"init-d": {"cmd-d"},
		"init-e": {"cmd-e"},
		"init-f": {"cmd-f"},
		"cmd-a":  {"hub"},
	}}
	nodes := &fakeNodes{metas: map[string]ports.NodeMeta{
		"cmd-a": {NodeID: "cmd-a"}, "hub": {NodeID: "hub"},
		"init-a": {NodeID: "init-a"}, "init-b": {NodeID: "init-b"},
		"init-c": {NodeID: "init-c"}, "init-d": {NodeID: "init-d"},
		"init-e": {NodeID: "init-e"}, "init-f": {NodeID: "init-f"},
	}}
	return edges, nodes
}

// blastFrom runs Of from cmd-a on a service built with the given configured
// default hub threshold and returns the set of node IDs reached.
func blastFrom(t *testing.T, defaultHub int) map[string]bool {
	t.Helper()
	edges, nodes := hubFixture()
	s, err := blastradius.NewService(edges, nodes, nil,
		blastradius.WithDefaultHubDegreeThreshold(defaultHub))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	resp, err := s.Of(context.Background(), "r", "main", []string{"cmd-a"},
		blastradius.Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range resp.Entries {
		ids[e.NodeID] = true
	}
	return ids
}

// TestOf_ConfiguredDefaultHubGates guards: WithDefaultHubDegreeThreshold
// supplies the gate value when a per-call Options leaves HubDegreeThreshold at 0.
func TestOf_ConfiguredDefaultHubGates(t *testing.T) {
	ids := blastFrom(t, 3) // hub has 6 neighbors > 3 → gated
	for _, sib := range []string{"init-b", "init-c", "init-d", "init-e", "init-f"} {
		if ids[sib] {
			t.Errorf("configured default should gate hub: %s should be absent", sib)
		}
	}
}

// TestOf_ConfiguredDefaultHubNegativeDisables guards: a negative
// configured default disables the gate with no per-call override.
func TestOf_ConfiguredDefaultHubNegativeDisables(t *testing.T) {
	ids := blastFrom(t, -1)
	for _, sib := range []string{"init-b", "init-c", "init-d", "init-e", "init-f"} {
		if !ids[sib] {
			t.Errorf("negative configured default should disable gate; %s missing", sib)
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

// TestNewService_ErrorsOnNilDeps verifies the construction-time nil check
// returns a typed ErrMissingDependency for each required dep (edges, nodes).
// staging is optional and is NOT checked.
func TestNewService_ErrorsOnNilDeps(t *testing.T) {
	t.Parallel()
	edges := &fakeEdges{}
	nodes := &fakeNodes{}
	cases := []struct {
		name  string
		edges ports.EdgeReader
		nodes ports.NodeLookup
	}{
		{"nil edges", nil, nodes},
		{"nil nodes", edges, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := blastradius.NewService(tc.edges, tc.nodes, nil)
			if s != nil {
				t.Errorf("expected nil *Service for %s, got %v", tc.name, s)
			}
			if !errors.Is(err, blastradius.ErrMissingDependency) {
				t.Errorf("expected ErrMissingDependency for %s, got %v", tc.name, err)
			}
		})
	}
}

// TestNewService_HappyPath verifies a nil staging is acceptable and the
// constructor returns a non-nil Service with nil error.
func TestNewService_HappyPath(t *testing.T) {
	t.Parallel()
	s, err := blastradius.NewService(&fakeEdges{}, &fakeNodes{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil *Service")
	}
}
