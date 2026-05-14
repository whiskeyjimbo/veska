package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// deadCodeFixture seeds repos, nodes, and edges for the dead-code adapter tests.
type deadCodeFixture struct {
	db     *sql.DB
	repoID string
	branch string
}

func setupDeadCodeFixture(t *testing.T) *deadCodeFixture {
	t.Helper()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))

	repoID := "repo1"
	branch := "main"
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return &deadCodeFixture{db: db, repoID: repoID, branch: branch}
}

func (f *deadCodeFixture) insertNode(t *testing.T, nodeID, filePath, kind, name string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, f.branch, f.repoID, "go", kind, name, filePath,
		1, 10, "h-"+nodeID, time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func (f *deadCodeFixture) insertEdge(t *testing.T, edgeID, src, dst, kind string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		edgeID, f.branch, f.repoID, src, dst, kind, "high", time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insert edge %s: %v", edgeID, err)
	}
}

func TestDeadCodeRepo_ReturnsOnlyNodesWithZeroInbound(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)

	// Three nodes in two files in scope; one node in scope has inbound; one is dead;
	// one in another file is dead but out-of-scope; one has only outbound (still dead).
	f.insertNode(t, "n-called", "pkg/a.go", "function", "called")
	f.insertNode(t, "n-dead", "pkg/a.go", "function", "deadHelper")
	f.insertNode(t, "n-caller", "pkg/a.go", "function", "caller")
	f.insertNode(t, "n-out-of-scope-dead", "pkg/c.go", "function", "outOfScopeDead")

	// caller -> called  (so n-called has 1 inbound; n-caller has 0 inbound and so is dead too)
	f.insertEdge(t, "e1", "n-caller", "n-called", "calls")

	repo := sqlite.NewDeadCodeRepo(f.db)
	got, err := repo.DeadNodesInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go", "pkg/b.go"})
	if err != nil {
		t.Fatalf("DeadNodesInFiles: %v", err)
	}

	gotIDs := make([]string, 0, len(got))
	for _, n := range got {
		gotIDs = append(gotIDs, n.NodeID)
	}
	sort.Strings(gotIDs)
	want := []string{"n-caller", "n-dead"}
	if !equalStrings(gotIDs, want) {
		t.Errorf("dead nodes = %v, want %v", gotIDs, want)
	}
}

func TestDeadCodeRepo_EmptyFilePathsReturnsEmpty(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)
	f.insertNode(t, "n-x", "pkg/a.go", "function", "helper")

	repo := sqlite.NewDeadCodeRepo(f.db)
	got, err := repo.DeadNodesInFiles(context.Background(), f.repoID, f.branch, nil)
	if err != nil {
		t.Fatalf("DeadNodesInFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero results for empty file paths, got %d", len(got))
	}
}

func TestDeadCodeRepo_BranchIsolation(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)

	// Insert a node and an inbound edge ON A DIFFERENT BRANCH; the node on `main`
	// must still be considered dead because the inbound edge is on `feature`.
	f.insertNode(t, "n-target", "pkg/a.go", "function", "target")

	// node + edge on feature branch
	_, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"n-target", "feature", f.repoID, "go", "function", "target", "pkg/a.go",
		1, 10, "h2", time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert feature-branch node: %v", err)
	}
	_, err = f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"n-caller-f", "feature", f.repoID, "go", "function", "callerF", "pkg/a.go",
		1, 10, "h3", time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert feature-branch caller: %v", err)
	}
	_, err = f.db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"e-f", "feature", f.repoID, "n-caller-f", "n-target", "calls", "high", time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insert feature-branch edge: %v", err)
	}

	repo := sqlite.NewDeadCodeRepo(f.db)
	got, err := repo.DeadNodesInFiles(context.Background(), f.repoID, "main", []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("DeadNodesInFiles: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "n-target" {
		t.Errorf("expected n-target dead on main branch, got %+v", got)
	}
}

func TestDeadCodeRepo_PopulatesNodeRefFields(t *testing.T) {
	t.Parallel()
	f := setupDeadCodeFixture(t)
	f.insertNode(t, "n-x", "pkg/a.go", "function", "helper")

	repo := sqlite.NewDeadCodeRepo(f.db)
	got, err := repo.DeadNodesInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("DeadNodesInFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	n := got[0]
	if n.NodeID != "n-x" || n.FilePath != "pkg/a.go" || n.Kind != "function" || n.Name != "helper" {
		t.Errorf("NodeRef fields wrong: %+v", n)
	}
	if n.LineStart != 1 || n.LineEnd != 10 {
		t.Errorf("line range = %d..%d, want 1..10", n.LineStart, n.LineEnd)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
