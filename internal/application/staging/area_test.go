package staging

import (
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// helpers

func mustNode(t *testing.T, id, path, name string, kind domain.NodeKind) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return n
}

func mustEdge(t *testing.T, src, tgt domain.NodeID, kind domain.EdgeKind) *domain.Edge {
	t.Helper()
	e, err := domain.NewEdge(domain.EdgeSpec{Src: src, Tgt: tgt, Kind: kind})
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	return e
}

// TestNewArea_IsEmpty verifies that a freshly-constructed Area
// holds no state (lossy-by-design: no persistence across constructions).
func TestNewArea_IsEmpty(t *testing.T) {
	sa := NewArea()

	nodes, ok := sa.GetStagedNodes("repo1", "main", "foo.go")
	if ok {
		t.Fatal("expected no staged nodes in fresh Area")
	}
	if nodes != nil {
		t.Fatal("expected nil nodes slice from empty Area")
	}

	edges, ok := sa.GetStagedEdges("repo1", "main", "foo.go")
	if ok {
		t.Fatal("expected no staged edges in fresh Area")
	}
	if edges != nil {
		t.Fatal("expected nil edges slice from empty Area")
	}

	files := sa.StagedFiles("repo1", "main")
	if len(files) != 0 {
		t.Fatalf("expected 0 staged files, got %d", len(files))
	}
}

// TestStageFile_RoundTrip verifies that staged nodes and edges are immediately
// visible via GetStagedNodes / GetStagedEdges (overlay read).
func TestStageFile_RoundTrip(t *testing.T) {
	sa := NewArea()

	n := mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction)
	e := mustEdge(t, "n1", "n2", domain.EdgeCalls)

	sa.Stage("repo1", "main", "pkg/foo.go", File{Nodes: []*domain.Node{n}, Edges: []*domain.Edge{e}})

	nodes, ok := sa.GetStagedNodes("repo1", "main", "pkg/foo.go")
	if !ok {
		t.Fatal("expected staged nodes to be present")
	}
	if len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}

	edges, ok := sa.GetStagedEdges("repo1", "main", "pkg/foo.go")
	if !ok {
		t.Fatal("expected staged edges to be present")
	}
	if len(edges) != 1 || edges[0].Src != "n1" {
		t.Fatalf("unexpected edges: %+v", edges)
	}
}

// TestStageFile_Replace verifies that staging a file twice replaces the first entry.
func TestStageFile_Replace(t *testing.T) {
	sa := NewArea()

	n1 := mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction)
	n2 := mustNode(t, "n2", "pkg/foo.go", "Bar", domain.KindFunction)

	sa.Stage("repo1", "main", "pkg/foo.go", File{Nodes: []*domain.Node{n1}, Edges: nil})
	sa.Stage("repo1", "main", "pkg/foo.go", File{Nodes: []*domain.Node{n2}, Edges: nil})

	nodes, ok := sa.GetStagedNodes("repo1", "main", "pkg/foo.go")
	if !ok {
		t.Fatal("expected staged nodes after second stage")
	}
	if len(nodes) != 1 || nodes[0].ID != "n2" {
		t.Fatalf("expected only n2 after replace, got: %+v", nodes)
	}
}

// TestFiles_ListsPaths verifies that Files returns all staged paths
// for a given repo+branch combination, without paths from other branches.
func TestFiles_ListsPaths(t *testing.T) {
	sa := NewArea()

	sa.Stage("repo1", "main", "a.go", File{Nodes: nil, Edges: nil})
	sa.Stage("repo1", "main", "b.go", File{Nodes: nil, Edges: nil})
	sa.Stage("repo1", "feat/x", "c.go", File{Nodes: nil, Edges: nil}) // different branch
	sa.Stage("repo2", "main", "d.go", File{Nodes: nil, Edges: nil})   // different repo

	files := sa.StagedFiles("repo1", "main")
	if len(files) != 2 {
		t.Fatalf("expected 2 files for repo1/main, got %d: %v", len(files), files)
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
	}
	if !seen["a.go"] || !seen["b.go"] {
		t.Fatalf("missing expected paths: %v", files)
	}
}

// TestDeleteFile verifies that a file is removed from staging after deletion.
func TestDeleteFile(t *testing.T) {
	sa := NewArea()

	n := mustNode(t, "n1", "pkg/foo.go", "Foo", domain.KindFunction)
	sa.Stage("repo1", "main", "pkg/foo.go", File{Nodes: []*domain.Node{n}, Edges: nil})

	sa.DeleteStagedFile("repo1", "main", "pkg/foo.go")

	_, ok := sa.GetStagedNodes("repo1", "main", "pkg/foo.go")
	if ok {
		t.Fatal("expected node to be absent after DeleteFile")
	}
	files := sa.StagedFiles("repo1", "main")
	if len(files) != 0 {
		t.Fatalf("expected 0 files after delete, got %d", len(files))
	}
}

// TestClear verifies that Clear removes all staged state for a branch.
func TestClear(t *testing.T) {
	sa := NewArea()

	sa.Stage("repo1", "main", "a.go", File{Nodes: nil, Edges: nil})
	sa.Stage("repo1", "main", "b.go", File{Nodes: nil, Edges: nil})
	sa.Stage("repo1", "feat/x", "c.go", File{Nodes: nil, Edges: nil}) // different branch - must survive

	sa.Clear("repo1", "main")

	if files := sa.StagedFiles("repo1", "main"); len(files) != 0 {
		t.Fatalf("expected 0 files after Clear, got %d: %v", len(files), files)
	}
	// other branch must be unaffected
	if files := sa.StagedFiles("repo1", "feat/x"); len(files) != 1 {
		t.Fatalf("expected feat/x to survive Clear, got %d files", len(files))
	}
}

// TestSnapshot returns a copy of staged nodes keyed by filePath.
func TestSnapshot(t *testing.T) {
	sa := NewArea()

	n1 := mustNode(t, "n1", "a.go", "A", domain.KindFunction)
	n2 := mustNode(t, "n2", "b.go", "B", domain.KindFunction)
	sa.Stage("repo1", "main", "a.go", File{Nodes: []*domain.Node{n1}, Edges: nil})
	sa.Stage("repo1", "main", "b.go", File{Nodes: []*domain.Node{n2}, Edges: nil})
	sa.Stage("repo1", "feat/x", "c.go", File{Nodes: nil, Edges: nil}) // different branch

	snap := sa.Snapshot("repo1", "main")
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries in snapshot, got %d", len(snap))
	}
	if snap["a.go"].Nodes[0].ID != "n1" {
		t.Fatalf("unexpected node in snapshot for a.go: %+v", snap["a.go"])
	}
	if snap["b.go"].Nodes[0].ID != "n2" {
		t.Fatalf("unexpected node in snapshot for b.go: %+v", snap["b.go"])
	}
}

// TestSnapshot_IsCopy verifies that mutating the snapshot does not affect staging.
func TestSnapshot_IsCopy(t *testing.T) {
	sa := NewArea()

	n := mustNode(t, "n1", "a.go", "A", domain.KindFunction)
	sa.Stage("repo1", "main", "a.go", File{Nodes: []*domain.Node{n}, Edges: nil})

	snap := sa.Snapshot("repo1", "main")
	delete(snap, "a.go")

	// Original must still be present.
	nodes, ok := sa.GetStagedNodes("repo1", "main", "a.go")
	if !ok || len(nodes) == 0 {
		t.Fatal("snapshot mutation affected staging state")
	}
}

// TestLosy_NewInstance verifies the lossy-across-restart guarantee:
// constructing a new Area starts empty regardless of previous instance.
func TestLossy_NewInstance(t *testing.T) {
	sa1 := NewArea()
	n := mustNode(t, "n1", "a.go", "A", domain.KindFunction)
	sa1.Stage("repo1", "main", "a.go", File{Nodes: []*domain.Node{n}, Edges: nil})

	// Simulate daemon restart by creating a new instance.
	sa2 := NewArea()
	_, ok := sa2.GetStagedNodes("repo1", "main", "a.go")
	if ok {
		t.Fatal("new Area must not inherit state from prior instance (lossy)")
	}
}

// TestStageFile_Concurrent verifies that concurrent StageFile / GetStagedNodes
// calls do not race (detected by -race).
func TestStageFile_Concurrent(t *testing.T) {
	sa := NewArea()

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers * 2)

	for i := range workers {
		go func(i int) {
			defer wg.Done()
			n, _ := domain.NewNode(domain.NodeSpec{ID: "n", Path: "f.go", Name: "F", Kind: domain.KindFunction})
			sa.Stage("repo", "main", "f.go", File{Nodes: []*domain.Node{n}, Edges: nil})
			_ = i
		}(i)
		go func() {
			defer wg.Done()
			sa.GetStagedNodes("repo", "main", "f.go")
		}()
	}
	wg.Wait()
}

// TestFiles_EmptySlice verifies Files returns an empty (non-nil) slice
// when nothing is staged for a repo+branch.
func TestFiles_EmptySlice(t *testing.T) {
	sa := NewArea()
	files := sa.StagedFiles("repo1", "main")
	if files == nil {
		t.Fatal("Files must return non-nil empty slice, not nil")
		return
	}
}

// TestOverlay_MissDoesNotMutate verifies that a cache miss (ok==false) returns
// nil slices without altering the store.
func TestOverlay_MissDoesNotMutate(t *testing.T) {
	sa := NewArea()

	nodes, ok := sa.GetStagedNodes("repo1", "main", "missing.go")
	if ok || nodes != nil {
		t.Fatal("miss must return nil, false")
	}
	edges, ok := sa.GetStagedEdges("repo1", "main", "missing.go")
	if ok || edges != nil {
		t.Fatal("miss must return nil, false")
	}
	// Verify store is still empty.
	if files := sa.StagedFiles("repo1", "main"); len(files) != 0 {
		t.Fatalf("miss must not create entries, got %d files", len(files))
	}
}
