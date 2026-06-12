package diffgate_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
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

// guardEphemeral builds an Ephemeral whose candidate overlay (file a.go) holds
// the given nodes and edges, against a base that knows exactly existingIDs (so
// every overlay node NOT in existingIDs is treated as new). Every overlay node
// is a changed node (mustNode sets no content hash).
func guardEphemeral(t *testing.T, nodes []*domain.Node, edges []*domain.Edge, existingIDs ...string) *diffgate.Ephemeral {
	t.Helper()
	metas := make(map[string]ports.NodeMeta, len(existingIDs))
	for _, id := range existingIDs {
		metas[id] = ports.NodeMeta{NodeID: id, FilePath: "a.go"}
	}
	base := &fakeBaseGraph{metas: metas}
	return indexCandidate(t, base, &domain.ParseResult{Nodes: nodes, Edges: edges})
}

func resolvedEdge(t *testing.T, src, tgt string) *domain.Edge {
	t.Helper()
	e, err := domain.NewEdge(
		domain.EdgeSpec{Src: domain.NodeID(src), Tgt: domain.NodeID(tgt), Kind: domain.EdgeCalls},
		domain.WithConfidence(domain.Definite),
	)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	return e
}

// TestGuard_Contained covers AC1: a change confined to the anchor and an
// existing in-radius caller reports contained.
func TestGuard_Contained(t *testing.T) {
	eph := guardEphemeral(t,
		[]*domain.Node{mustNode(t, "pkg:Anchor", "a.go", "Anchor"), mustNode(t, "pkg:Caller1", "a.go", "Caller1")},
		nil,
		"pkg:Anchor", "pkg:Caller1", // both pre-exist in base
	)
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Anchor": {"pkg:Caller1"}}})
	v, err := g.Check(context.Background(), eph, "pkg:Anchor", blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Contained || len(v.Offending) != 0 {
		t.Fatalf("verdict = %+v, want contained", v)
	}
}

// TestGuard_ExceededExistingDistant covers AC2: a modified EXISTING node
// outside the radius is real scope creep → offending.
func TestGuard_ExceededExistingDistant(t *testing.T) {
	eph := guardEphemeral(t,
		[]*domain.Node{mustNode(t, "pkg:Anchor", "a.go", "Anchor"), mustNode(t, "pkg:Far", "a.go", "Far")},
		nil,
		"pkg:Anchor", "pkg:Far", // pkg:Far is existing distant code
	)
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Anchor": {}}})
	v, err := g.Check(context.Background(), eph, "pkg:Anchor", blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v.Contained || !slices.Contains(v.Offending, "pkg:Far") {
		t.Fatalf("verdict = %+v, want exceeded listing pkg:Far", v)
	}
}

// TestGuard_NewNodeWiredContained is the heart of ll57.5: the canonical
// dead-code fix adds a NEW caller wired (resolved edge) to the dead anchor.
// The new node is admitted into the allowed set → contained, even though the
// dead anchor's base radius is empty.
func TestGuard_NewNodeWiredContained(t *testing.T) {
	eph := guardEphemeral(t,
		[]*domain.Node{mustNode(t, "pkg:Dead", "a.go", "Dead"), mustNode(t, "pkg:Caller", "a.go", "Caller")},
		[]*domain.Edge{resolvedEdge(t, "pkg:Caller", "pkg:Dead")},
		"pkg:Dead", // only the anchor pre-exists; pkg:Caller is NEW
	)
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Dead": {}}})
	v, err := g.Check(context.Background(), eph, "pkg:Dead", blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Contained || len(v.Offending) != 0 {
		t.Fatalf("verdict = %+v, want contained (new caller wired to the anchor)", v)
	}
}

// TestGuard_NewNodeWiredTransitively: a chain of new nodes (Caller→Helper) all
// wired back to the anchor is admitted to a fixpoint.
func TestGuard_NewNodeWiredTransitively(t *testing.T) {
	eph := guardEphemeral(t,
		[]*domain.Node{
			mustNode(t, "pkg:Dead", "a.go", "Dead"),
			mustNode(t, "pkg:Caller", "a.go", "Caller"),
			mustNode(t, "pkg:Helper", "a.go", "Helper"),
		},
		[]*domain.Edge{
			resolvedEdge(t, "pkg:Caller", "pkg:Dead"),
			resolvedEdge(t, "pkg:Caller", "pkg:Helper"),
		},
		"pkg:Dead",
	)
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Dead": {}}})
	v, err := g.Check(context.Background(), eph, "pkg:Dead", blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !v.Contained {
		t.Fatalf("verdict = %+v, want contained (Helper reached transitively via Caller)", v)
	}
}

// TestGuard_NewNodeUnwiredOffending: a NEW node NOT connected to the allowed
// set is unrelated new code → scope creep → offending. This is the
// safe-direction guard against admitting arbitrary new code.
func TestGuard_NewNodeUnwiredOffending(t *testing.T) {
	eph := guardEphemeral(t,
		[]*domain.Node{mustNode(t, "pkg:Dead", "a.go", "Dead"), mustNode(t, "pkg:Orphan", "a.go", "Orphan")},
		nil, // no edge wiring Orphan to anything
		"pkg:Dead",
	)
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Dead": {}}})
	v, err := g.Check(context.Background(), eph, "pkg:Dead", blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v.Contained || !slices.Contains(v.Offending, "pkg:Orphan") {
		t.Fatalf("verdict = %+v, want exceeded listing pkg:Orphan (unwired new code)", v)
	}
}

// TestGuard_UnresolvedEdgeDoesNotAdmit: a new node wired only by an UNRESOLVED
// edge is not admitted (connectivity can't be confirmed → over-block).
func TestGuard_UnresolvedEdgeDoesNotAdmit(t *testing.T) {
	unresolved, err := domain.NewEdge(domain.EdgeSpec{Src: "pkg:Caller", Tgt: "pkg:Dead", Kind: domain.EdgeCalls})
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	if unresolved.Resolved {
		t.Fatalf("expected an unresolved edge (default confidence)")
	}
	eph := guardEphemeral(t,
		[]*domain.Node{mustNode(t, "pkg:Dead", "a.go", "Dead"), mustNode(t, "pkg:Caller", "a.go", "Caller")},
		[]*domain.Edge{unresolved},
		"pkg:Dead",
	)
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Dead": {}}})
	v, err := g.Check(context.Background(), eph, "pkg:Dead", blastradius.Options{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if v.Contained || !slices.Contains(v.Offending, "pkg:Caller") {
		t.Fatalf("verdict = %+v, want exceeded (unresolved edge can't confirm connectivity)", v)
	}
}

// TestGuard_TruncatedPropagated: a clipped radius traversal is surfaced.
func TestGuard_TruncatedPropagated(t *testing.T) {
	eph := guardEphemeral(t, []*domain.Node{mustNode(t, "pkg:Anchor", "a.go", "Anchor")}, nil, "pkg:Anchor")
	g := newGuard(t, &fakeRadius{reachable: map[string][]string{"pkg:Anchor": {}}, truncated: true})
	v, err := g.Check(context.Background(), eph, "pkg:Anchor", blastradius.Options{})
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
	eph := guardEphemeral(t, []*domain.Node{mustNode(t, "pkg:Anchor", "a.go", "Anchor")}, nil, "pkg:Anchor")
	g := newGuard(t, &fakeRadius{})
	if _, err := g.Check(context.Background(), nil, "pkg:Anchor", blastradius.Options{}); !errors.Is(err, diffgate.ErrMissingDependency) {
		t.Fatalf("nil ephemeral should error with ErrMissingDependency, got %v", err)
	}
	if _, err := g.Check(context.Background(), eph, "", blastradius.Options{}); err == nil {
		t.Fatalf("empty anchor should error (file-anchored findings unsupported)")
	}
	g2 := newGuard(t, &fakeRadius{err: errors.New("boom")})
	if _, err := g2.Check(context.Background(), eph, "pkg:Anchor", blastradius.Options{}); err == nil {
		t.Fatalf("radius error should propagate")
	}
}
