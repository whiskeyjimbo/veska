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
}

func (f *fakeStore) ClonedNodes(_ context.Context, _, _ string, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	f.gotExclude = excludeKinds
	return f.rows, f.err
}

func TestNewFinder_NilStore(t *testing.T) {
	t.Parallel()
	if _, err := duplicates.NewFinder(nil); !errors.Is(err, duplicates.ErrMissingDependency) {
		t.Fatalf("want ErrMissingDependency, got %v", err)
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
	finder, err := duplicates.NewFinder(store)
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

func TestExactClones_DropsSingletons(t *testing.T) {
	t.Parallel()
	// A store that (defensively) hands back a singleton hash must not yield a
	// group: the Finder enforces >=2 locally.
	store := &fakeStore{rows: []duplicates.ClonedNode{
		{ContentHash: "lonely", NodeID: "n1", FilePath: "a.go", Kind: "function"},
	}}
	finder, _ := duplicates.NewFinder(store)
	groups, err := finder.ExactClones(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("ExactClones: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("want 0 groups for a singleton, got %d", len(groups))
	}
}
