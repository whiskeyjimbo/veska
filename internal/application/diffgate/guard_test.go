package diffgate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
)

// fakeRadius is a deterministic blastradius view: it returns a fixed reachable
// set for the seed, standing in for the real service over a fixture graph.
type fakeRadius struct {
	// reachable maps a seed node ID to the node IDs in its blast radius
	// (excluding the seed itself; the Guard adds the anchor explicitly).
	reachable map[string][]string
	truncated bool
	err       error
}

func (f *fakeRadius) Of(_ context.Context, _, _ string, seedIDs []string, _ blastradius.Options) (blastradius.Response, error) {
	if f.err != nil {
		return blastradius.Response{}, f.err
	}
	resp := blastradius.Response{Truncated: f.truncated}
	for _, s := range seedIDs {
		resp.Entries = append(resp.Entries, blastradius.Entry{NodeID: s, Distance: 0})
		for _, r := range f.reachable[s] {
			resp.Entries = append(resp.Entries, blastradius.Entry{NodeID: r, Distance: 1})
		}
	}
	return resp, nil
}

func newGuard(t *testing.T, r diffgate.BlastRadius) *diffgate.Guard {
	t.Helper()
	g, err := diffgate.NewGuard(r)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}

// TestGuard_Contained covers AC1: a change confined to the anchor and its
// immediate blast radius reports contained.
func TestGuard_Contained(t *testing.T) {
	radius := &fakeRadius{reachable: map[string][]string{
		"pkg:Anchor": {"pkg:Caller1", "pkg:Caller2"},
	}}
	g := newGuard(t, radius)

	// The diff touched the anchor and one of its callers — both allowed.
	changed := []string{"pkg:Anchor", "pkg:Caller1"}
	v, err := g.Check(context.Background(), testRepo, testBranch, "pkg:Anchor", changed, blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Contained {
		t.Fatalf("verdict = %+v, want contained", v)
	}
	if len(v.Offending) != 0 {
		t.Fatalf("offending = %v, want none", v.Offending)
	}
}

// TestGuard_Exceeded covers AC2: a change touching nodes outside the radius
// reports exceeded and lists exactly the offending nodes.
func TestGuard_Exceeded(t *testing.T) {
	radius := &fakeRadius{reachable: map[string][]string{
		"pkg:Anchor": {"pkg:Caller1"},
	}}
	g := newGuard(t, radius)

	// Anchor + an in-radius caller are fine; pkg:Far and pkg:AlsoFar are not.
	changed := []string{"pkg:Anchor", "pkg:Caller1", "pkg:Far", "pkg:AlsoFar"}
	v, err := g.Check(context.Background(), testRepo, testBranch, "pkg:Anchor", changed, blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v.Contained {
		t.Fatalf("verdict = %+v, want exceeded", v)
	}
	want := []string{"pkg:AlsoFar", "pkg:Far"} // sorted
	if len(v.Offending) != len(want) || v.Offending[0] != want[0] || v.Offending[1] != want[1] {
		t.Fatalf("offending = %v, want %v (sorted)", v.Offending, want)
	}
}

// TestGuard_AnchorAloneIsContained: a change touching only the anchor is
// always contained, even with an empty radius.
func TestGuard_AnchorAloneIsContained(t *testing.T) {
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{}})
	v, err := g.Check(context.Background(), testRepo, testBranch, "pkg:Anchor", []string{"pkg:Anchor"}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Contained {
		t.Fatalf("anchor-only change should be contained; got %+v", v)
	}
}

// TestGuard_TruncatedPropagated: a clipped radius traversal is surfaced so an
// exceeded verdict can be caveated.
func TestGuard_TruncatedPropagated(t *testing.T) {
	radius := &fakeRadius{reachable: map[string][]string{"pkg:Anchor": {}}, truncated: true}
	g := newGuard(t, radius)
	v, err := g.Check(context.Background(), testRepo, testBranch, "pkg:Anchor", []string{"pkg:Far"}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Truncated {
		t.Fatalf("expected Truncated to propagate; got %+v", v)
	}
}

func TestGuard_Errors(t *testing.T) {
	if _, err := diffgate.NewGuard(nil); !errors.Is(err, diffgate.ErrMissingDependency) {
		t.Fatalf("NewGuard(nil) = %v, want ErrMissingDependency", err)
	}
	g := newGuard(t, &fakeRadius{})
	if _, err := g.Check(context.Background(), testRepo, testBranch, "", nil, blastradius.Options{}); err == nil {
		t.Fatalf("empty anchor should error (file-anchored findings unsupported)")
	}
	g2 := newGuard(t, &fakeRadius{err: errors.New("boom")})
	if _, err := g2.Check(context.Background(), testRepo, testBranch, "pkg:Anchor", nil, blastradius.Options{}); err == nil {
		t.Fatalf("radius error should propagate")
	}
}
