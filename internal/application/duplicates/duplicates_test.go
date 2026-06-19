// SPDX-License-Identifier: AGPL-3.0-only

package duplicates_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
)

type fakeStore struct {
	rows []duplicates.ClonedNode
	err  error
	// gotExclude captures the excludeKinds the Finder forwarded.
	gotExclude []string

	// structural-side fixtures (Type-2).
	structuralRows   []duplicates.ClonedNode
	gotStructExclude []string

	// captured CloneQuery scoping (repo/branch/path).
	gotQuery       duplicates.CloneQuery
	gotStructQuery duplicates.CloneQuery

	// near-side fixtures.
	edges       []duplicates.SimilarEdge
	gotMinScore float32
	gotNearExcl []string
}

func (f *fakeStore) ClonedNodes(_ context.Context, q duplicates.CloneQuery, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	f.gotExclude = excludeKinds
	f.gotQuery = q
	return f.rows, f.err
}

func (f *fakeStore) StructuralNodes(_ context.Context, q duplicates.CloneQuery, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	f.gotStructExclude = excludeKinds
	f.gotStructQuery = q
	return f.structuralRows, f.err
}

func (f *fakeStore) SimilarEdges(_ context.Context, _, _ string, minScore float32, excludeKinds []string) ([]duplicates.SimilarEdge, error) {
	f.gotMinScore = minScore
	f.gotNearExcl = excludeKinds
	return f.edges, f.err
}

func TestNewFinder_NilStores(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	if _, err := duplicates.NewFinder(nil, store, ""); !errors.Is(err, duplicates.ErrMissingDependency) {
		t.Fatalf("nil clone store: want ErrMissingDependency, got %v", err)
	}
	if _, err := duplicates.NewFinder(store, nil, ""); !errors.Is(err, duplicates.ErrMissingDependency) {
		t.Fatalf("nil near store: want ErrMissingDependency, got %v", err)
	}
}

func TestExactClones_GroupsAndOrders(t *testing.T) {
	t.Parallel()
	store := &fakeStore{rows: []duplicates.ClonedNode{
		// hashBig: 3 members, intentionally out of file/line order.
		{ContentHash: "hashBig", NodeID: "n3", FilePath: "z.go", LineStart: 5, Kind: "function"},
		{ContentHash: "hashBig", NodeID: "n1", FilePath: "a.go", LineStart: 9, Kind: "function"},
		{ContentHash: "hashBig", NodeID: "n2", FilePath: "a.go", LineStart: 1, Kind: "function"},
		// hashSmall: 2 members.
		{ContentHash: "hashSmall", NodeID: "m1", FilePath: "b.go", LineStart: 1, Kind: "method"},
		{ContentHash: "hashSmall", NodeID: "m2", FilePath: "c.go", LineStart: 1, Kind: "method"},
	}}
	finder, err := duplicates.NewFinder(store, store, "")
	if err != nil {
		t.Fatalf("NewFinder: %v", err)
	}

	groups, err := finder.ExactClones(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("ExactClones: %v", err)
	}

	// The Finder must forward the canonical exclusion set to the store.
	if len(store.gotExclude) != len(duplicates.ExcludedKinds) {
		t.Fatalf("forwarded exclude %v, want %v", store.gotExclude, duplicates.ExcludedKinds)
	}

	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	// Largest group first.
	if groups[0].ContentHash != "hashBig" || groups[0].Size != 3 {
		t.Fatalf("group[0] = %+v, want hashBig size 3", groups[0])
	}
	if groups[1].ContentHash != "hashSmall" || groups[1].Size != 2 {
		t.Fatalf("group[1] = %+v, want hashSmall size 2", groups[1])
	}
	// Members sorted by (FilePath, LineStart): a.go:1, a.go:9, z.go:5.
	wantOrder := []string{"n2", "n1", "n3"}
	for i, w := range wantOrder {
		if groups[0].Members[i].NodeID != w {
			t.Fatalf("group[0] member order %d = %q, want %q", i, groups[0].Members[i].NodeID, w)
		}
	}
}

func mem(id, file string, line int) duplicates.CloneMember {
	return duplicates.CloneMember{NodeID: id, FilePath: file, LineStart: line, Kind: "function"}
}

func TestNearDuplicates_ClustersTransitively(t *testing.T) {
	t.Parallel()
	// Two components: {a,b,c} chained (a~b, b~c), and {x,y} separate.
	store := &fakeStore{edges: []duplicates.SimilarEdge{
		{Src: mem("a", "a.go", 1), Dst: mem("b", "b.go", 1), Score: 0.90},
		{Src: mem("b", "b.go", 1), Dst: mem("c", "c.go", 1), Score: 0.82},
		{Src: mem("x", "x.go", 1), Dst: mem("y", "y.go", 1), Score: 0.95},
	}}
	finder, _ := duplicates.NewFinder(store, store, "")

	clusters, err := finder.NearDuplicates(context.Background(), "r1", "main", 0.80)
	if err != nil {
		t.Fatalf("NearDuplicates: %v", err)
	}
	if store.gotMinScore != 0.80 {
		t.Errorf("forwarded minScore %v, want 0.80", store.gotMinScore)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d", len(clusters))
	}
	// Largest first: {a,b,c}, size 3, MinScore 0.82 (the weakest link).
	if clusters[0].Size != 3 {
		t.Fatalf("cluster[0] size = %d, want 3", clusters[0].Size)
	}
	if clusters[0].MinScore != 0.82 || clusters[0].MaxScore != 0.90 {
		t.Errorf("cluster[0] score bounds = [%v,%v], want [0.82,0.90]", clusters[0].MinScore, clusters[0].MaxScore)
	}
	gotIDs := []string{clusters[0].Members[0].NodeID, clusters[0].Members[1].NodeID, clusters[0].Members[2].NodeID}
	want := []string{"a", "b", "c"} // sorted by (file,line): a.go, b.go, c.go
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("cluster[0] member order %v, want %v", gotIDs, want)
		}
	}
}

func TestNearDuplicates_DefaultsThreshold(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	finder, _ := duplicates.NewFinder(store, store, "")
	if _, err := finder.NearDuplicates(context.Background(), "r1", "main", 0); err != nil {
		t.Fatalf("NearDuplicates: %v", err)
	}
	// Empty embedderID -> DefaultNearThreshold fallback.
	if store.gotMinScore != duplicates.DefaultNearThreshold {
		t.Errorf("minScore=0 (no embedder) should default to %v, forwarded %v", duplicates.DefaultNearThreshold, store.gotMinScore)
	}
	if len(store.gotNearExcl) != len(duplicates.ExcludedKinds) {
		t.Errorf("forwarded near exclude %v, want %v", store.gotNearExcl, duplicates.ExcludedKinds)
	}
}

func TestNearDuplicates_DefaultIsEmbedderCalibrated(t *testing.T) {
	t.Parallel()
	// A Finder built for a known embedder must apply that embedder's calibrated
	// default (not the global fallback) when min_score is omitted.
	store := &fakeStore{}
	finder, _ := duplicates.NewFinder(store, store, "nomic-embed-text")
	if _, err := finder.NearDuplicates(context.Background(), "r1", "main", 0); err != nil {
		t.Fatalf("NearDuplicates: %v", err)
	}
	want := duplicates.NearThresholdFor("nomic-embed-text")
	if store.gotMinScore != want {
		t.Errorf("nomic default minScore = %v, want calibrated %v", store.gotMinScore, want)
	}
	if want == duplicates.DefaultNearThreshold {
		t.Fatalf("test is vacuous: nomic threshold equals the fallback %v", want)
	}
}

func TestNearThresholdFor(t *testing.T) {
	t.Parallel()
	if got := duplicates.NearThresholdFor("definitely-not-an-embedder"); got != duplicates.DefaultNearThreshold {
		t.Errorf("unknown embedder = %v, want fallback %v", got, duplicates.DefaultNearThreshold)
	}
	// An Ollama:tag must resolve to the bare-name calibration, not the fallback.
	if got, want := duplicates.NearThresholdFor("nomic-embed-text:latest"), duplicates.NearThresholdFor("nomic-embed-text"); got != want {
		t.Errorf("tagged nomic = %v, want bare-name calibration %v", got, want)
	}
	// Every calibrated value must exceed autolink's related cutoff (0.60): a
	// near-duplicate is at least as similar as "merely related".
	for _, id := range []string{"model2vec(potion-code-16M)", "nomic-embed-text", "veska-static-v2"} {
		if got := duplicates.NearThresholdFor(id); got <= 0.60 {
			t.Errorf("%s threshold %v must exceed autolink related cutoff 0.60", id, got)
		}
	}
}

func TestExactClones_DropsSingletons(t *testing.T) {
	t.Parallel()
	// A store that (defensively) hands back a singleton hash must not yield a
	// group: the Finder enforces >=2 locally.
	store := &fakeStore{rows: []duplicates.ClonedNode{
		{ContentHash: "lonely", NodeID: "n1", FilePath: "a.go", Kind: "function"},
	}}
	finder, _ := duplicates.NewFinder(store, store, "")
	groups, err := finder.ExactClones(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("ExactClones: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("want 0 groups for a singleton, got %d", len(groups))
	}
}

func TestStructuralClones_GroupsByStructuralHashAndExcludesContainers(t *testing.T) {
	t.Parallel()
	store := &fakeStore{structuralRows: []duplicates.ClonedNode{
		// Two renamed copies sharing one structural_hash → a Type-2 group.
		{StructuralHash: "structA", ContentHash: "c1", NodeID: "n1", FilePath: "a.go", LineStart: 9, Kind: "function"},
		{StructuralHash: "structA", ContentHash: "c2", NodeID: "n2", FilePath: "b.go", LineStart: 1, Kind: "function"},
		// A singleton structural_hash must not form a group.
		{StructuralHash: "structB", ContentHash: "c3", NodeID: "n3", FilePath: "c.go", LineStart: 1, Kind: "method"},
	}}
	finder, _ := duplicates.NewFinder(store, store, "")
	groups, err := finder.StructuralClones(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("StructuralClones: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 structural group, got %d", len(groups))
	}
	if groups[0].Size != 2 {
		t.Errorf("want size 2, got %d", groups[0].Size)
	}
	// The container-kind exclusion list is forwarded to the store.
	if len(store.gotStructExclude) == 0 {
		t.Error("StructuralClones should forward ExcludedKinds to the store")
	}
}
