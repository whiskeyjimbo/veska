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

func seedNodeRow(t *testing.T, db *sql.DB, repoID, branch, nodeID, symbolPath, filePath, kind string, lineStart, lineEnd int) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, branch, repoID, "go", kind, symbolPath, filePath,
		lineStart, lineEnd, "h", now, "test", "system"); err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func openLookupTestDB(t *testing.T) (*sql.DB, *sqlite.NodeLookupRepo) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, sqlite.NewNodeLookupRepo(db)
}

func TestNodeLookupRepo_ReturnsKnownNodes_DropsMissing(t *testing.T) {
	t.Parallel()
	db, repo := openLookupTestDB(t)

	seedNodeRow(t, db, "r1", "main", "n1", "pkg.A", "a.go", "function", 1, 10)
	seedNodeRow(t, db, "r1", "main", "n2", "pkg.B", "b.go", "method", 20, 30)

	got, err := repo.LookupNodes(context.Background(), "r1", "main", []string{"n1", "missing", "n2"})
	if err != nil {
		t.Fatalf("LookupNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(got), got)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].NodeID < got[j].NodeID })
	if got[0].NodeID != "n1" || got[0].SymbolPath != "pkg.A" || got[0].FilePath != "a.go" ||
		got[0].Kind != "function" || got[0].LineStart != 1 || got[0].LineEnd != 10 {
		t.Errorf("n1 mismatch: %+v", got[0])
	}
	if got[1].NodeID != "n2" || got[1].SymbolPath != "pkg.B" || got[1].FilePath != "b.go" ||
		got[1].Kind != "method" || got[1].LineStart != 20 || got[1].LineEnd != 30 {
		t.Errorf("n2 mismatch: %+v", got[1])
	}
}

func TestNodeLookupRepo_BranchIsolated(t *testing.T) {
	t.Parallel()
	db, repo := openLookupTestDB(t)

	seedNodeRow(t, db, "r1", "main", "n1", "pkg.A", "a.go", "function", 1, 10)
	seedNodeRow(t, db, "r1", "feature", "n1", "pkg.A2", "a2.go", "function", 5, 15)

	got, err := repo.LookupNodes(context.Background(), "r1", "main", []string{"n1"})
	if err != nil {
		t.Fatalf("LookupNodes: %v", err)
	}
	if len(got) != 1 || got[0].SymbolPath != "pkg.A" {
		t.Fatalf("expected main-branch row, got %+v", got)
	}
}

func TestNodeLookupRepo_EmptyInput(t *testing.T) {
	t.Parallel()
	_, repo := openLookupTestDB(t)

	got, err := repo.LookupNodes(context.Background(), "r1", "main", nil)
	if err != nil {
		t.Fatalf("LookupNodes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

func TestNodeLookupRepo_NodesInFile_ReturnsMatchingNodes(t *testing.T) {
	t.Parallel()
	db, repo := openLookupTestDB(t)

	seedNodeRow(t, db, "r1", "main", "n1", "pkg.A", "x.go", "function", 1, 10)
	seedNodeRow(t, db, "r1", "main", "n2", "pkg.B", "x.go", "method", 11, 20)
	seedNodeRow(t, db, "r1", "main", "n3", "pkg.C", "y.go", "function", 1, 5)

	got, err := repo.NodesInFile(context.Background(), "r1", "main", "x.go")
	if err != nil {
		t.Fatalf("NodesInFile: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "n1" || got[1] != "n2" {
		t.Fatalf("expected [n1 n2], got %v", got)
	}
}

func TestNodeLookupRepo_NodesInFile_UnknownFile(t *testing.T) {
	t.Parallel()
	db, repo := openLookupTestDB(t)
	seedNodeRow(t, db, "r1", "main", "n1", "pkg.A", "x.go", "function", 1, 10)

	got, err := repo.NodesInFile(context.Background(), "r1", "main", "does/not/exist.go")
	if err != nil {
		t.Fatalf("NodesInFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero results, got %v", got)
	}
}

func TestNodeLookupRepo_NodesInFile_BranchAndRepoIsolated(t *testing.T) {
	t.Parallel()
	db, repo := openLookupTestDB(t)

	seedNodeRow(t, db, "r1", "main", "n1", "pkg.A", "x.go", "function", 1, 10)
	seedNodeRow(t, db, "r1", "feature", "n2", "pkg.A", "x.go", "function", 1, 10)
	seedNodeRow(t, db, "r2", "main", "n3", "pkg.A", "x.go", "function", 1, 10)

	got, err := repo.NodesInFile(context.Background(), "r1", "main", "x.go")
	if err != nil {
		t.Fatalf("NodesInFile: %v", err)
	}
	if len(got) != 1 || got[0] != "n1" {
		t.Fatalf("expected only n1 from r1/main, got %v", got)
	}
}

func TestNodeLookupRepo_NodesInFile_EmptyPath(t *testing.T) {
	t.Parallel()
	_, repo := openLookupTestDB(t)
	got, err := repo.NodesInFile(context.Background(), "r1", "main", "")
	if err != nil {
		t.Fatalf("NodesInFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected nil result for empty path, got %v", got)
	}
}

// TestNodeLookupRepo_RepoIsolated verifies that query isolation prevents node IDs
// from leaking across repositories since node IDs are not globally unique.
func TestNodeLookupRepo_RepoIsolated(t *testing.T) {
	t.Parallel()
	db, repo := openLookupTestDB(t)

	seedNodeRow(t, db, "r1", "main", "n1", "pkg.A", "a.go", "function", 1, 10)
	seedNodeRow(t, db, "r2", "main", "n2", "pkg.Z", "z.go", "function", 50, 60)

	got, err := repo.LookupNodes(context.Background(), "r2", "main", []string{"n1", "n2"})
	if err != nil {
		t.Fatalf("LookupNodes: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "n2" || got[0].SymbolPath != "pkg.Z" {
		t.Fatalf("expected only r2's n2, got %+v", got)
	}
}
