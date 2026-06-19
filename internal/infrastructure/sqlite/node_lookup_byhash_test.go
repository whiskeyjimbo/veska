// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// byHashFixture inserts test nodes with specific content hashes to verify
// NodesByContentHash filtering behavior.
type byHashFixture struct {
	db     *sql.DB
	repoID string
	branch string
}

func setupByHashFixture(t *testing.T) *byHashFixture {
	t.Helper()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	repoID, branch := "repo1", "main"
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return &byHashFixture{db: db, repoID: repoID, branch: branch}
}

func (f *byHashFixture) insert(t *testing.T, nodeID, filePath, kind, hash string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, f.branch, f.repoID, "go", kind, nodeID, filePath,
		1, 10, hash, time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func TestNodesByContentHash_MatchesHashAndExcludesKinds(t *testing.T) {
	t.Parallel()
	f := setupByHashFixture(t)
	f.insert(t, "n1", "a.go", "function", "H")
	f.insert(t, "n2", "b.go", "function", "H")
	f.insert(t, "n3", "c.go", "field", "H")
	f.insert(t, "n4", "d.go", "function", "OTHER")

	repo := sqlite.NewNodeLookupRepo(f.db)
	got, err := repo.NodesByContentHash(context.Background(), f.repoID, f.branch, "H", duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("NodesByContentHash: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.NodeID)
	}
	sort.Strings(ids)
	if want := []string{"n1", "n2"}; !equalStrings(ids, want) {
		t.Errorf("nodes for hash H = %v, want %v (field n3 excluded, OTHER-hash n4 excluded)", ids, want)
	}
}

func TestNodesByContentHash_EmptyHashNoOp(t *testing.T) {
	t.Parallel()
	f := setupByHashFixture(t)
	f.insert(t, "n1", "a.go", "function", "")
	repo := sqlite.NewNodeLookupRepo(f.db)
	got, err := repo.NodesByContentHash(context.Background(), f.repoID, f.branch, "", duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("NodesByContentHash: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty hash returned %d rows, want 0", len(got))
	}
}

func TestNodesByContentHash_HydratesRef(t *testing.T) {
	t.Parallel()
	f := setupByHashFixture(t)
	f.insert(t, "n1", "pkg/a.go", "function", "H")
	repo := sqlite.NewNodeLookupRepo(f.db)
	got, err := repo.NodesByContentHash(context.Background(), f.repoID, f.branch, "H", duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("NodesByContentHash: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	n := got[0]
	if n.NodeID != "n1" || n.FilePath != "pkg/a.go" || n.Kind != "function" || n.ContentHash != "H" {
		t.Errorf("ref = %+v, want id=n1 file=pkg/a.go kind=function hash=H", n)
	}
}
