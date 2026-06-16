package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// wikiFixtureService constructs a HotZoneService pre-populated with fixture data for MCP handler tests.
func wikiFixtureService(t *testing.T) *wiki.HotZoneService {
	t.Helper()
	edges := &blastFakeEdges{inbound: map[string][]string{
		"a": {"x", "y"},
		"c": {"x", "y"},
	}}
	nodes := &blastFakeNodes{
		metas: map[string]ports.NodeMeta{
			"a": {NodeID: "a"}, "b": {NodeID: "b"}, "c": {NodeID: "c"},
			"x": {NodeID: "x"}, "y": {NodeID: "y"},
		},
		byFile: map[string][]string{
			"a.go": {"a"}, "b.go": {"b"}, "c.go": {"c"},
		},
	}
	blast, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	counts := func(context.Context, string) (map[string]int, error) {
		return map[string]int{"a.go": 5, "b.go": 5, "c.go": 3}, nil
	}
	svc, err := wiki.NewHotZoneService(counts, nodes.NodesInFile, blast)
	if err != nil {
		t.Fatalf("NewHotZoneService: %v", err)
	}
	return svc
}

func dispatchHotZone(t *testing.T, r *Registry, params any) (HotZoneResponse, *RPCError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := &Request{Method: "eng_get_hot_zone", Params: json.RawMessage(raw)}
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		return HotZoneResponse{}, rpcErr
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var resp HotZoneResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp, nil
}

// TestHotZone_ReturnsRankedData ensures the tool returns the same ranked data that the wiki page exposes.
func TestHotZone_ReturnsRankedData(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }

	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot, nil)

	resp, rpcErr := dispatchHotZone(t, r, map[string]any{
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Zones) != 3 {
		t.Fatalf("expected 3 zones, got %d (%+v)", len(resp.Zones), resp.Zones)
	}
	// The returned zone paths are absolute, but the wiki markdown page retains relative path shapes.
	if resp.Zones[0].FilePath != "/tmp/r/a.go" || resp.Zones[0].Score != 15 {
		t.Errorf("zone[0]: got %+v, want /tmp/r/a.go score 15", resp.Zones[0])
	}
	if resp.Zones[2].FilePath != "/tmp/r/b.go" {
		t.Errorf("zone[2]: got %+v, want /tmp/r/b.go", resp.Zones[2])
	}

	// Verify that the returned zone data matches the ranked zones modulo path differences.
	rep, err := svc.Rank(context.Background(), "r1", "main", "/tmp/r")
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	for i := range rep.Zones {
		got := resp.Zones[i]
		want := rep.Zones[i]
		want.FilePath = "/tmp/r/" + want.FilePath
		if got != want {
			t.Errorf("zone %d diverges: tool=%+v page=%+v", i, got, want)
		}
	}
}

// TestHotZone_EmptyZonesSurfacesDegradedReason ensures that when ranking yields no results due to
// an empty commit window, a degraded reason and hint are returned.
func TestHotZone_EmptyZonesSurfacesDegradedReason(t *testing.T) {
	edges := &blastFakeEdges{}
	nodes := &blastFakeNodes{metas: map[string]ports.NodeMeta{}, byFile: map[string][]string{}}
	blast, err := blastradius.NewService(edges, nodes, nil)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	emptyCounts := func(context.Context, string) (map[string]int, error) { return map[string]int{}, nil }
	svc, err := wiki.NewHotZoneService(emptyCounts, nodes.NodesInFile, blast)
	if err != nil {
		t.Fatalf("NewHotZoneService: %v", err)
	}
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }

	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot, nil)

	resp, rpcErr := dispatchHotZone(t, r, map[string]any{
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.Zones) != 0 {
		t.Fatalf("expected zero zones, got %d", len(resp.Zones))
	}
	if len(resp.DegradedReasons) != 1 || resp.DegradedReasons[0] != "no_recent_commits" {
		t.Fatalf("expected degraded_reasons=[no_recent_commits], got %v", resp.DegradedReasons)
	}
	if resp.Hint == "" {
		t.Fatalf("expected non-empty hint on empty zones, got %q", resp.Hint)
	}
}

// TestHotZone_NonEmptyZonesNoDegradedReason verifies that no degraded reason is returned when zones are populated.
func TestHotZone_NonEmptyZonesNoDegradedReason(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }

	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot, nil)

	resp, rpcErr := dispatchHotZone(t, r, map[string]any{
		"repo_id": "r1",
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("err: %+v", rpcErr)
	}
	if len(resp.DegradedReasons) != 0 {
		t.Errorf("expected no degraded_reasons when zones present, got %v", resp.DegradedReasons)
	}
}

// TestHotZone_AcceptsShortID ensures that short repo ID prefixes are successfully resolved during hot zone lookup.
func TestHotZone_AcceptsShortID(t *testing.T) {
	const fullID = "62d72fa222a0193f8fa927f95dd6a3575c7566964c8b8f6ba14aafc5a1ea871f"
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }
	repos := &fakeRepoLister{recs: []application.RepoRecord{{RepoID: fullID, RootPath: "/tmp/r", ActiveBranch: "main"}}}

	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot, repos)

	resp, rpcErr := dispatchHotZone(t, r, map[string]any{
		"repo_id": ShortRepoID(fullID),
		"branch":  "main",
	})
	if rpcErr != nil {
		t.Fatalf("short_id rejected: %+v", rpcErr)
	}
	if len(resp.Zones) != 3 {
		t.Fatalf("expected 3 zones via short_id, got %d", len(resp.Zones))
	}
}

func TestHotZone_RequiresParams(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) { return "/tmp/r", nil }
	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot, nil)

	_, rpcErr := dispatchHotZone(t, r, map[string]any{"repo_id": "r1"})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("expected InvalidParams, got %+v", rpcErr)
	}
}

func TestHotZone_UnknownRepo(t *testing.T) {
	svc := wikiFixtureService(t)
	repoRoot := func(context.Context, string) (string, error) {
		return "", fmt.Errorf("no such repo")
	}
	r := NewRegistry()
	RegisterWikiTools(r, svc, repoRoot, nil)

	_, rpcErr := dispatchHotZone(t, r, map[string]any{"repo_id": "ghost", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %+v", rpcErr)
	}
}

func TestHotZone_NotWiredReturnsInternalError(t *testing.T) {
	r := NewRegistry()
	RegisterWikiTools(r, nil, nil, nil)

	_, rpcErr := dispatchHotZone(t, r, map[string]any{"repo_id": "r1", "branch": "main"})
	if rpcErr == nil || rpcErr.Code != CodeInternalError {
		t.Fatalf("expected InternalError, got %+v", rpcErr)
	}
}
