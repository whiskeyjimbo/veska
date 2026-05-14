package repo_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/repo"
)

const createReposTable = `
CREATE TABLE repos (
	repo_id          TEXT PRIMARY KEY,
	root_path        TEXT NOT NULL UNIQUE,
	added_at         INTEGER NOT NULL,
	active_branch    TEXT,
	last_promoted_sha TEXT,
	module_path      TEXT
)`

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if _, err := db.Exec(createReposTable); err != nil {
		t.Fatalf("create repos table: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newGitRepo creates a temp directory with a .git/hooks/ subdirectory.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
	return dir
}

func TestAddRepo(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if repoID == "" {
		t.Fatal("repoID is empty")
	}

	var rootPath string
	err = db.QueryRow("SELECT root_path FROM repos WHERE repo_id = ?", repoID).Scan(&rootPath)
	if err != nil {
		t.Fatalf("query row: %v", err)
	}

	canonical, _ := filepath.EvalSymlinks(dir)
	if rootPath != canonical {
		t.Errorf("root_path = %q, want %q", rootPath, canonical)
	}
}

func TestAddRepoIdempotent(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	id1, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	id2, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if id1 != id2 {
		t.Errorf("idempotent: id1=%s id2=%s differ", id1, id2)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM repos WHERE repo_id = ?", id1).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestAddRepoReadsGoMod(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	gomod := "module github.com/foo/bar\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	repoID, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var modPath sql.NullString
	if err := db.QueryRow("SELECT module_path FROM repos WHERE repo_id = ?", repoID).Scan(&modPath); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !modPath.Valid || modPath.String != "github.com/foo/bar" {
		t.Errorf("module_path = %v, want github.com/foo/bar", modPath)
	}
}

func TestAddRepoReadsPackageJSON(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	pkgJSON, _ := json.Marshal(map[string]string{"name": "@scope/pkg", "version": "1.0.0"})
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkgJSON, 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	repoID, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var modPath sql.NullString
	if err := db.QueryRow("SELECT module_path FROM repos WHERE repo_id = ?", repoID).Scan(&modPath); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !modPath.Valid || modPath.String != "@scope/pkg" {
		t.Errorf("module_path = %v, want @scope/pkg", modPath)
	}
}

func TestAddRepoInstallsHooks(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	if _, err := repo.Add(context.Background(), db, dir); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, hook := range []string{"post-commit", "post-checkout"} {
		hookPath := filepath.Join(dir, ".git", "hooks", hook)
		info, err := os.Stat(hookPath)
		if err != nil {
			t.Errorf("hook %s not found: %v", hook, err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("hook %s is not executable (mode %v)", hook, info.Mode())
		}
	}
}

func TestRemoveRepo(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := repo.Remove(context.Background(), db, repoID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM repos WHERE repo_id = ?", repoID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after Remove, got %d", count)
	}
}

func TestRemoveRepoRemovesHooks(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	repoID, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Confirm hooks are present first.
	for _, hook := range []string{"post-commit", "post-checkout"} {
		if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", hook)); err != nil {
			t.Fatalf("hook %s missing after Add: %v", hook, err)
		}
	}

	if err := repo.Remove(context.Background(), db, repoID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, hook := range []string{"post-commit", "post-checkout"} {
		hookPath := filepath.Join(dir, ".git", "hooks", hook)
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Errorf("hook %s still exists after Remove (err=%v)", hook, err)
		}
	}
}
