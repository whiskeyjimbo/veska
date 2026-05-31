package composition

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// createReposTable mirrors the post-migration repos schema so the resolver can
// exercise the real column set without a full migration run.
const createReposTable = `
CREATE TABLE repos (
	repo_id          TEXT PRIMARY KEY,
	root_path        TEXT NOT NULL UNIQUE,
	added_at         INTEGER NOT NULL,
	active_branch    TEXT,
	last_promoted_sha TEXT,
	module_path      TEXT,
	kind             TEXT NOT NULL DEFAULT 'tracked',
	canonical_url    TEXT,
	last_accessed_at INTEGER,
	prompted_at      INTEGER
);
CREATE UNIQUE INDEX idx_repos_canonical_url
	ON repos(canonical_url)
	WHERE canonical_url IS NOT NULL;
CREATE TABLE repo_aliases (
	name     TEXT PRIMARY KEY,
	repo_id  TEXT NOT NULL,
	FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_repo_aliases_repo_id ON repo_aliases(repo_id);`

func newReposDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqldriver.Name, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if _, err := db.Exec(createReposTable); err != nil {
		t.Fatalf("create repos table: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
	return dir
}

func TestRepoRootByID_ResolvesRegisteredRepo(t *testing.T) {
	db := newReposDB(t)
	dir := newGitRepo(t)

	repoID, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	root, err := RepoRootByID(db)(context.Background(), repoID)
	if err != nil {
		t.Fatalf("RepoRootByID: %v", err)
	}
	if root != dir {
		t.Fatalf("root = %q, want %q", root, dir)
	}
}

func TestRepoRootByID_UnknownRepoErrors(t *testing.T) {
	db := newReposDB(t)

	_, err := RepoRootByID(db)(context.Background(), "deadbeef")
	if err == nil {
		t.Fatal("expected error for unregistered repo, got nil")
	}
	if !strings.Contains(err.Error(), "repo root lookup: repo \"deadbeef\" is not registered") {
		t.Fatalf("unexpected error: %v", err)
	}
}
