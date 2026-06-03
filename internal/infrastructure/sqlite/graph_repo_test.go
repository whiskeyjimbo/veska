package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// openGraphRepoTestDBWithHandle is openGraphRepoTestDB but also returns the
// underlying *sql.DB so tests can assert directly on table columns the read
// path does not hydrate (e.g. nodes.snippet).
func openGraphRepoTestDBWithHandle(t *testing.T) (*sqlite.GraphRepo, *sql.DB) {
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
	return sqlite.NewGraphRepo(db, db), db
}

// openGraphRepoTestDB opens an isolated DB with the real migrated schema,
// seeds a repos row, and returns a constructed GraphRepo. The same *sql.DB
// handle backs both the read and write side — the driver serialises access
// internally, which is sufficient for a single-connection test.
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
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind}, opts...)
	if err != nil {
		t.Fatalf("NewNode(NodeSpec{ID: %s}): %v", id, err)
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
		return
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
		return
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

// TestGraphRepo_FindNodeIDsByPrefix_Unique verifies a prefix that matches
// exactly one node resolves to its full id, and a full id resolves to itself
// (solov2-uej9.3 — `veska node <12-char display id>`).
func TestGraphRepo_FindNodeIDsByPrefix_Unique(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()
	full := "f470f8ff4243aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := r.SaveNode(ctx, "r1", "main", mustNode(t, full, "a.go", "Add", domain.KindFunction)); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}
	got, err := r.FindNodeIDsByPrefix(ctx, "f470f8ff4243", 2)
	if err != nil {
		t.Fatalf("FindNodeIDsByPrefix: %v", err)
	}
	if len(got) != 1 || string(got[0]) != full {
		t.Fatalf("prefix lookup = %v; want [%s]", got, full)
	}
	// A full id is its own unique prefix.
	gotFull, err := r.FindNodeIDsByPrefix(ctx, full, 2)
	if err != nil {
		t.Fatalf("FindNodeIDsByPrefix(full): %v", err)
	}
	if len(gotFull) != 1 || string(gotFull[0]) != full {
		t.Fatalf("full-id lookup = %v; want [%s]", gotFull, full)
	}
}

// TestGraphRepo_FindNodeIDsByPrefix_MultiBranchNotAmbiguous pins the DISTINCT
// requirement: the SAME node_id stored on two branches must count as one
// candidate, not two, so a unique display prefix is not misread as ambiguous
// (solov2-uej9.3).
func TestGraphRepo_FindNodeIDsByPrefix_MultiBranchNotAmbiguous(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()
	full := "abc123def456aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, br := range []string{"main", "develop"} {
		if err := r.SaveNode(ctx, "r1", br, mustNode(t, full, "a.go", "Add", domain.KindFunction)); err != nil {
			t.Fatalf("SaveNode on %s: %v", br, err)
		}
	}
	got, err := r.FindNodeIDsByPrefix(ctx, "abc123def456", 2)
	if err != nil {
		t.Fatalf("FindNodeIDsByPrefix: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("multi-branch prefix lookup = %v (len %d); want 1 distinct id", got, len(got))
	}
}

// TestGraphRepo_FindNodeIDsByPrefix_Ambiguous verifies two distinct node_ids
// sharing a prefix both surface (capped at limit) so the handler can detect
// ambiguity (solov2-uej9.3).
func TestGraphRepo_FindNodeIDsByPrefix_Ambiguous(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()
	ids := []string{
		"dead000011110000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"dead000022220000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for i, id := range ids {
		if err := r.SaveNode(ctx, "r1", "main", mustNode(t, id, "a.go", "Fn", domain.KindFunction)); err != nil {
			t.Fatalf("SaveNode %d: %v", i, err)
		}
	}
	got, err := r.FindNodeIDsByPrefix(ctx, "dead0000", 2)
	if err != nil {
		t.Fatalf("FindNodeIDsByPrefix: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ambiguous prefix lookup = %v (len %d); want 2", got, len(got))
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

// TestGraphRepo_FindNodes_UnqualifiedSuffix pins solov2-d2x: an unqualified
// name matches the trailing segment of a qualified symbol_path, so "Start"
// finds "Server.Start" instead of silently returning nothing. Exact matches
// still sort ahead of suffix matches.
func TestGraphRepo_FindNodes_UnqualifiedSuffix(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	for _, n := range []*domain.Node{
		mustNode(t, "n1", "a.go", "Server.Start", domain.KindMethod),
		mustNode(t, "n2", "b.go", "Client.Start", domain.KindMethod),
		mustNode(t, "n3", "c.go", "Start", domain.KindFunction),
		mustNode(t, "n4", "d.go", "Restart", domain.KindFunction), // must NOT match
	} {
		if err := r.SaveNode(ctx, "r1", "main", n); err != nil {
			t.Fatalf("SaveNode %s: %v", n.ID, err)
		}
	}

	got, err := r.FindNodes(ctx, "r1", "main", "Start")
	if err != nil {
		t.Fatalf("FindNodes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("FindNodes(Start) = %d nodes (%v); want 3 (Start, Server.Start, Client.Start)", len(got), names(got))
	}
	// The exact match sorts first (ORDER BY exact DESC).
	if got[0].Name != "Start" {
		t.Errorf("expected exact match 'Start' first, got %q", got[0].Name)
	}
	for _, n := range got {
		if n.Name == "Restart" {
			t.Errorf("FindNodes(Start) wrongly matched 'Restart' — '.' anchor missing")
		}
	}
}

// TestGraphRepo_FindNodes_CaseSensitive guards solov2-xcb1: identifier
// matching is byte-exact. SQLite LIKE is case-insensitive for ASCII by
// default, so before the COLLATE BINARY fix, searching "Run" also matched
// "FSNotifyWatcher.run" — a different symbol. Go (and most supported
// languages) treats "Run" and "run" as distinct identifiers.
func TestGraphRepo_FindNodes_CaseSensitive(t *testing.T) {
	t.Parallel()
	r := openGraphRepoTestDB(t)
	ctx := context.Background()

	for _, n := range []*domain.Node{
		mustNode(t, "n1", "a.go", "Server.Run", domain.KindMethod),          // matches "Run"
		mustNode(t, "n2", "b.go", "FSNotifyWatcher.run", domain.KindMethod), // distinct lowercase — must NOT match "Run"
	} {
		if err := r.SaveNode(ctx, "r1", "main", n); err != nil {
			t.Fatalf("SaveNode %s: %v", n.ID, err)
		}
	}

	got, err := r.FindNodes(ctx, "r1", "main", "Run")
	if err != nil {
		t.Fatalf("FindNodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FindNodes(Run) = %d nodes (%v); want 1 (Server.Run only)", len(got), names(got))
	}
	if got[0].Name != "Server.Run" {
		t.Errorf("expected Server.Run, got %q", got[0].Name)
	}
}

func names(ns []*domain.Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Name
	}
	return out
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

	e, _ := domain.NewEdge(domain.EdgeSpec{Src: "src", Tgt: "tgt", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Definite))
	if err := r.SaveEdge(ctx, "r1", "main", e); err != nil {
		t.Fatalf("SaveEdge: %v", err)
	}

	g, err := r.LoadGraph(ctx, "r1", "main")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if g == nil {
		t.Fatal("LoadGraph returned nil graph")
		return
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
	e, _ := domain.NewEdge(domain.EdgeSpec{Src: "src", Tgt: "tgt", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Probable))
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
	e, _ := domain.NewEdge(domain.EdgeSpec{Src: "a1", Tgt: "a2", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Definite))
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
		return
	}
	if _, ok := g.Node("anything"); ok {
		t.Error("empty graph reported a node")
	}
}

// snippetOf reads the raw nodes.snippet column for a node — the read path does
// not hydrate it, so tests must query it directly.
func snippetOf(t *testing.T, db *sql.DB, repoID, branch, id string) sql.NullString {
	t.Helper()
	var s sql.NullString
	err := db.QueryRow(
		`SELECT snippet FROM nodes WHERE repo_id = ? AND branch = ? AND node_id = ?`,
		repoID, branch, id).Scan(&s)
	if err != nil {
		t.Fatalf("query snippet for %q: %v", id, err)
	}
	return s
}

// TestGraphRepo_SaveNode_PersistsRawContentSnippet verifies a node saved with
// RawContent stores that body in nodes.snippet, and a node without RawContent
// stores SQL NULL.
func TestGraphRepo_SaveNode_PersistsRawContentSnippet(t *testing.T) {
	t.Parallel()
	r, db := openGraphRepoTestDBWithHandle(t)
	ctx := context.Background()

	body := "func Alpha() { return 42 }"
	withBody := mustNode(t, "n1", "pkg/a.go", "Alpha", domain.KindFunction,
		domain.WithRawContent(body))
	without := mustNode(t, "n2", "pkg/b.go", "Beta", domain.KindFunction)

	if err := r.SaveNode(ctx, "r1", "main", withBody); err != nil {
		t.Fatalf("SaveNode(withBody): %v", err)
	}
	if err := r.SaveNode(ctx, "r1", "main", without); err != nil {
		t.Fatalf("SaveNode(without): %v", err)
	}

	if got := snippetOf(t, db, "r1", "main", "n1"); !got.Valid || got.String != body {
		t.Errorf("snippet for n1 = %#v, want %q", got, body)
	}
	if got := snippetOf(t, db, "r1", "main", "n2"); got.Valid {
		t.Errorf("snippet for n2 = %#v, want NULL", got)
	}
}

// TestGraphRepo_SaveNode_CapsSnippetOnRuneBoundary verifies an over-limit body
// is capped at the byte limit on a UTF-8 rune boundary (no broken runes).
func TestGraphRepo_SaveNode_CapsSnippetOnRuneBoundary(t *testing.T) {
	t.Parallel()
	r, db := openGraphRepoTestDBWithHandle(t)
	ctx := context.Background()

	// 3-byte runes ensure the 2000-byte cap does not land on a boundary.
	body := strings.Repeat("世", 1000) // 3000 bytes
	n := mustNode(t, "n1", "pkg/a.go", "Alpha", domain.KindFunction,
		domain.WithRawContent(body))
	if err := r.SaveNode(ctx, "r1", "main", n); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}

	got := snippetOf(t, db, "r1", "main", "n1")
	if !got.Valid {
		t.Fatal("snippet is NULL, want capped body")
	}
	if !utf8.ValidString(got.String) {
		t.Error("capped snippet is not valid UTF-8")
	}
	if len(got.String) > 2000 {
		t.Errorf("capped snippet is %d bytes, want <= 2000", len(got.String))
	}
	if len(got.String) <= 2000-3 {
		t.Errorf("capped snippet is %d bytes, want as close to 2000 as a rune boundary allows", len(got.String))
	}
	if !strings.HasPrefix(body, got.String) {
		t.Error("capped snippet is not a prefix of the original body")
	}
}

// TestGraphRepo_SaveNode_RoundTripUnaffectedBySnippet verifies persisting a
// snippet does not change the GetNode/LoadGraph round-trip.
func TestGraphRepo_SaveNode_RoundTripUnaffectedBySnippet(t *testing.T) {
	t.Parallel()
	r, _ := openGraphRepoTestDBWithHandle(t)
	ctx := context.Background()

	in := mustNode(t, "n1", "pkg/a.go", "Alpha", domain.KindFunction,
		domain.WithLanguage("go"),
		domain.WithRawContent("func Alpha() {}"))
	if err := r.SaveNode(ctx, "r1", "main", in); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}

	got, err := r.GetNode(ctx, "r1", "main", "n1")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatal("GetNode returned nil for a saved node")
		return
	}
	if got.ID != in.ID || got.Path != in.Path || got.Name != in.Name || got.Kind != in.Kind {
		t.Errorf("GetNode round-trip mismatch: got %+v", got)
	}

	g, err := r.LoadGraph(ctx, "r1", "main")
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if _, ok := g.Node("n1"); !ok {
		t.Error("LoadGraph did not return the saved node")
	}
}
