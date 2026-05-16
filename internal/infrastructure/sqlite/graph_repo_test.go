package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// openGraphRepoTestDB opens an isolated DB with the real migrated schema,
// seeds a repos row, and returns a constructed GraphRepo. The same *sql.DB
// handle backs both the read and write side — modernc.org/sqlite serialises
// access internally, which is sufficient for a single-connection test.
func openGraphRepoTestDB(t *testing.T) *sqlite.GraphRepo {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", 1); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return sqlite.NewGraphRepo(db, db)
}

func mustNode(t *testing.T, id, path, name string, kind domain.NodeKind, opts ...domain.NodeOption) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(id, path, name, kind, opts...)
	if err != nil {
		t.Fatalf("NewNode(%s): %v", id, err)
	}
	return n
}

// TestGraphRepo_SaveNode_GetNode_RoundTrip verifies SaveNode followed by
// GetNode returns an equivalent node.
func TestGraphRepo_SaveNode_GetNode_RoundTrip(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	in := mustNode(t, "n1", "pkg/a.go", "Alpha", domain.KindFunction,
		domain.WithLanguage("go"),
		domain.WithLines(domain.LineRange{Start: 3, End: 9}),
		domain.WithSignature("func Alpha()"))

	if err := r.SaveNode(ctx, "r1", "main", in); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}

	got, err := r.GetNode(ctx, "r1", "main", "n1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatal("GetNode returned nil for a saved node")
	}
	if got.ID != in.ID || got.Path != in.Path || got.Name != in.Name || got.Kind != in.Kind {
		t.Errorf("node mismatch: got %+v want %+v", got, in)
	}
	if got.Language == nil || *got.Language != "go" {
		t.Errorf("Language = %v; want go", got.Language)
	}
	if got.Lines == nil || got.Lines.Start != 3 || got.Lines.End != 9 {
		t.Errorf("Lines = %v; want {3,9}", got.Lines)
	}
	if got.Signature == nil || *got.Signature != "func Alpha()" {
		t.Errorf("Signature = %v; want func Alpha()", got.Signature)
	}
}

// TestGraphRepo_SaveNode_Upserts verifies SaveNode replaces an existing row
// keyed on node ID rather than erroring or duplicating.
func TestGraphRepo_SaveNode_Upserts(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	if err := r.SaveNode(ctx, "r1", "main", mustNode(t, "n1", "a.go", "Alpha", domain.KindFunction)); err != nil {
		t.Fatalf("first SaveNode: %v", err)
	}
	if err := r.SaveNode(ctx, "r1", "main", mustNode(t, "n1", "a.go", "AlphaRenamed", domain.KindMethod)); err != nil {
		t.Fatalf("second SaveNode: %v", err)
	}
	got, err := r.GetNode(ctx, "r1", "main", "n1")
	if err != nil || got == nil {
		t.Fatalf("GetNode: %v / %v", got, err)
	}
	if got.Name != "AlphaRenamed" || got.Kind != domain.KindMethod {
		t.Errorf("upsert did not replace row: got %+v", got)
	}
}

// TestGraphRepo_GetNode_MissingReturnsNilNil verifies a miss is (nil, nil).
func TestGraphRepo_GetNode_MissingReturnsNilNil(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	got, err := r.GetNode(context.Background(), "r1", "main", "does-not-exist")
	if err != nil {
		t.Fatalf("GetNode: unexpected error %v", err)
	}
	if got != nil {
		t.Errorf("GetNode miss = %+v; want nil", got)
	}
}

// TestGraphRepo_FindNodes_ExactMatch verifies FindNodes returns only exact
// symbol-name matches.
func TestGraphRepo_FindNodes_ExactMatch(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	for _, n := range []*domain.Node{
		mustNode(t, "n1", "a.go", "Target", domain.KindFunction),
		mustNode(t, "n2", "b.go", "Target", domain.KindMethod),
		mustNode(t, "n3", "c.go", "Other", domain.KindFunction),
	} {
		if err := r.SaveNode(ctx, "r1", "main", n); err != nil {
			t.Fatalf("SaveNode %s: %v", n.ID, err)
		}
	}

	got, err := r.FindNodes(ctx, "r1", "main", "Target")
	if err != nil {
		t.Fatalf("FindNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("FindNodes(Target) = %d nodes; want 2", len(got))
	}
	for _, n := range got {
		if n.Name != "Target" {
			t.Errorf("FindNodes returned non-match %q", n.Name)
		}
	}

	none, err := r.FindNodes(ctx, "r1", "main", "Nope")
	if err != nil {
		t.Fatalf("FindNodes(Nope): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("FindNodes(Nope) = %d; want 0", len(none))
	}
}

// TestGraphRepo_SaveEdge_LoadGraph verifies SaveEdge then LoadGraph includes
// the edge with its endpoints.
func TestGraphRepo_SaveEdge_LoadGraph(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	if err := r.SaveNode(ctx, "r1", "main", mustNode(t, "src", "a.go", "Src", domain.KindFunction)); err != nil {
		t.Fatalf("SaveNode src: %v", err)
	}
	if err := r.SaveNode(ctx, "r1", "main", mustNode(t, "tgt", "b.go", "Tgt", domain.KindFunction)); err != nil {
		t.Fatalf("SaveNode tgt: %v", err)
	}

	e, _ := domain.NewEdge("src", "tgt", domain.EdgeCalls, domain.WithConfidence(domain.Definite))
	if err := r.SaveEdge(ctx, "r1", "main", e); err != nil {
		t.Fatalf("SaveEdge: %v", err)
	}

	g, err := r.LoadGraph(ctx, "r1", "main")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if g == nil {
		t.Fatal("LoadGraph returned nil graph")
	}
	if _, ok := g.Node("src"); !ok {
		t.Error("graph missing src node")
	}
	out := g.OutgoingEdges("src")
	if len(out) != 1 || out[0].Tgt != "tgt" || out[0].Kind != domain.EdgeCalls {
		t.Errorf("outgoing edges = %+v; want one CALLS->tgt", out)
	}
}

// TestGraphRepo_SaveEdge_Upserts verifies re-saving the same (From,To,Kind)
// edge does not duplicate or error.
func TestGraphRepo_SaveEdge_Upserts(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"src", "tgt"} {
		if err := r.SaveNode(ctx, "r1", "main", mustNode(t, id, id+".go", id, domain.KindFunction)); err != nil {
			t.Fatalf("SaveNode %s: %v", id, err)
		}
	}
	e, _ := domain.NewEdge("src", "tgt", domain.EdgeCalls, domain.WithConfidence(domain.Probable))
	if err := r.SaveEdge(ctx, "r1", "main", e); err != nil {
		t.Fatalf("first SaveEdge: %v", err)
	}
	if err := r.SaveEdge(ctx, "r1", "main", e); err != nil {
		t.Fatalf("second SaveEdge: %v", err)
	}
	g, err := r.LoadGraph(ctx, "r1", "main")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if got := len(g.OutgoingEdges("src")); got != 1 {
		t.Errorf("outgoing edges after re-save = %d; want 1", got)
	}
}

// TestGraphRepo_DeleteFile removes both nodes and edges of a file.
func TestGraphRepo_DeleteFile(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	// Two nodes in a.go, one in b.go, plus an edge between the a.go nodes.
	for _, n := range []*domain.Node{
		mustNode(t, "a1", "a.go", "A1", domain.KindFunction),
		mustNode(t, "a2", "a.go", "A2", domain.KindFunction),
		mustNode(t, "b1", "b.go", "B1", domain.KindFunction),
	} {
		if err := r.SaveNode(ctx, "r1", "main", n); err != nil {
			t.Fatalf("SaveNode %s: %v", n.ID, err)
		}
	}
	e, _ := domain.NewEdge("a1", "a2", domain.EdgeCalls, domain.WithConfidence(domain.Definite))
	if err := r.SaveEdge(ctx, "r1", "main", e); err != nil {
		t.Fatalf("SaveEdge: %v", err)
	}

	if err := r.DeleteFile(ctx, "r1", "main", "a.go"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	g, err := r.LoadGraph(ctx, "r1", "main")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if _, ok := g.Node("a1"); ok {
		t.Error("a1 still present after DeleteFile")
	}
	if _, ok := g.Node("a2"); ok {
		t.Error("a2 still present after DeleteFile")
	}
	if _, ok := g.Node("b1"); !ok {
		t.Error("b1 wrongly removed by DeleteFile(a.go)")
	}
	// The a1->a2 edge must be gone too.
	if got := len(g.OutgoingEdges("a1")); got != 0 {
		t.Errorf("edge survived DeleteFile: %d outgoing from a1", got)
	}
}

// TestGraphRepo_LoadGraph_UnknownReturnsEmptyNonNil verifies an unknown
// repo/branch yields a non-nil empty Graph, never nil.
func TestGraphRepo_LoadGraph_UnknownReturnsEmptyNonNil(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	g, err := r.LoadGraph(context.Background(), "r1", "no-such-branch")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if g == nil {
		t.Fatal("LoadGraph returned nil for unknown branch; want empty non-nil Graph")
	}
	if _, ok := g.Node("anything"); ok {
		t.Error("empty graph reported a node")
	}
}
