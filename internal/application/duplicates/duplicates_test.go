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

	// near-side fixtures.
	edges       []duplicates.SimilarEdge
	gotMinScore float32
	gotNearExcl []string
}

func (f *fakeStore) ClonedNodes(_ context.Context, _, _ string, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	f.gotExclude = excludeKinds
	return f.rows, f.err
}

func (f *fakeStore) SimilarEdges(_ context.Context, _, _ string, minScore float32, excludeKinds []string) ([]duplicates.SimilarEdge, error) {
	f.gotMinScore = minScore
	f.gotNearExcl = excludeKinds
	return f.edges, f.err
}

func TestNewFinder_NilStores(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	if _, err := duplicates.NewFinder(nil, store); !errors.Is(err, duplicates.ErrMissingDependency) {
		t.Fatalf("nil clone store: want ErrMissingDependency, got %v", err)
	}
	if _, err := duplicates.NewFinder(store, nil); !errors.Is(err, duplicates.ErrMissingDependency) {
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
	finder, err := duplicates.NewFinder(store, store)
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
	finder, _ := duplicates.NewFinder(store, store)

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
	finder, _ := duplicates.NewFinder(store, store)
	if _, err := finder.NearDuplicates(context.Background(), "r1", "main", 0); err != nil {
		t.Fatalf("NearDuplicates: %v", err)
	}
	if store.gotMinScore != duplicates.DefaultNearThreshold {
		t.Errorf("minScore=0 should default to %v, forwarded %v", duplicates.DefaultNearThreshold, store.gotMinScore)
	}
	if len(store.gotNearExcl) != len(duplicates.ExcludedKinds) {
		t.Errorf("forwarded near exclude %v, want %v", store.gotNearExcl, duplicates.ExcludedKinds)
	}
}

func TestExactClones_DropsSingletons(t *testing.T) {
	t.Parallel()
	// A store that (defensively) hands back a singleton hash must not yield a
	// group: the Finder enforces >=2 locally.
	store := &fakeStore{rows: []duplicates.ClonedNode{
		{ContentHash: "lonely", NodeID: "n1", FilePath: "a.go", Kind: "function"},
	}}
	finder, _ := duplicates.NewFinder(store, store)
	groups, err := finder.ExactClones(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("ExactClones: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("want 0 groups for a singleton, got %d", len(groups))
	}
}
