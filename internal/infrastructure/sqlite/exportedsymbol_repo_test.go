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

type exportedSymbolFixture struct {
	db     *sql.DB
	repoID string
	branch string
}

func setupExportedSymbolFixture(t *testing.T) *exportedSymbolFixture {
	t.Helper()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	repoID := "repo1"
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return &exportedSymbolFixture{db: db, repoID: repoID, branch: "main"}
}


func (f *exportedSymbolFixture) insertNode(t *testing.T, nodeID, filePath, kind, name string, exported bool) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
        exported
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, f.branch, f.repoID, "go", kind, name, filePath,
		1, 10, "h-"+nodeID, time.Now().UnixMilli(), "service:veska", "system",
		exported,
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func TestExportedSymbolRepo_FiltersExportedPublicSurface(t *testing.T) {
	t.Parallel()
	f := setupExportedSymbolFixture(t)

	f.insertNode(t, "n-fn", "pkg/a.go", "function", "Foo", true)
	f.insertNode(t, "n-m", "pkg/a.go", "method", "T.M", true)
	f.insertNode(t, "n-i", "pkg/a.go", "interface", "I", true)
	f.insertNode(t, "n-type", "pkg/a.go", "type", "T", true)
	f.insertNode(t, "n-struct", "pkg/a.go", "struct", "S", true)
	f.insertNode(t, "n-var", "pkg/a.go", "variable", "V", true)
	f.insertNode(t, "n-unexp", "pkg/a.go", "function", "helper", false)
	f.insertNode(t, "n-pkg", "pkg/a.go", "package", "p", true)
	f.insertNode(t, "n-oos", "pkg/other.go", "function", "X", true)

	repo := sqlite.NewExportedSymbolRepo(f.db)
	got, err := repo.ExportedSymbolsInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("ExportedSymbolsInFiles: %v", err)
	}
	ids := make([]string, len(got))
	for i, s := range got {
		ids[i] = s.NodeID
	}
	sort.Strings(ids)
	want := []string{"n-fn", "n-i", "n-m", "n-struct", "n-type", "n-var"}
	if len(ids) != len(want) {
		t.Fatalf("got %d, want %d: %+v", len(ids), len(want), got)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d]=%q, want %q", i, ids[i], want[i])
		}
	}
}

func TestExportedSymbolRepo_EmptyFilePathsIsNoOp(t *testing.T) {
	t.Parallel()
	f := setupExportedSymbolFixture(t)
	f.insertNode(t, "n-fn", "pkg/a.go", "function", "Foo", true)

	repo := sqlite.NewExportedSymbolRepo(f.db)
	got, err := repo.ExportedSymbolsInFiles(context.Background(), f.repoID, f.branch, nil)
	if err != nil {
		t.Fatalf("ExportedSymbolsInFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 rows for empty paths, got %d", len(got))
	}
}
