// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"database/sql"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// hashedNode bundles identifiers for a seeded node row to keep parameter lists short.
type hashedNode struct {
	repoID, branch, nodeID, kind, contentHash string
}

// seedHashedNode inserts a node row with an explicit content_hash and kind to verify clone grouping.
func seedHashedNode(t *testing.T, db *sql.DB, n hashedNode) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		n.repoID, "/tmp/"+n.repoID, now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.nodeID, n.branch, n.repoID, "go", n.kind, n.nodeID, n.nodeID+".go",
		1, 10, n.contentHash, now, "test", "system"); err != nil {
		t.Fatalf("insert node %s: %v", n.nodeID, err)
	}
}

// simEdge bundles identifiers for a seeded SIMILAR_TO edge, where a nil score inserts a NULL score representing a legacy edge.
type simEdge struct {
	repoID, branch, src, dst string
	score                    any
}

// seedSimilarEdge inserts a SIMILAR_TO edge between two existing nodes.
func seedSimilarEdge(t *testing.T, db *sql.DB, e simEdge) {
	t.Helper()
	now := time.Now().UnixMilli()
	edgeID := e.src + "->" + e.dst
	if _, err := db.Exec(`INSERT INTO edges (
		edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, score, last_promoted_at
	) VALUES (?,?,?,?,?,?,?,?,?)`,
		edgeID, e.branch, e.repoID, e.src, e.dst, "SIMILAR_TO", "unresolved", e.score, now); err != nil {
		t.Fatalf("insert edge %s: %v", edgeID, err)
	}
}

func openCloneTestDB(t *testing.T) (*sql.DB, *sqlite.CloneRepo) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, sqlite.NewCloneRepo(db)
}

// TestCloneRepo_GroupsSharedHashes verifies size-based grouping, kind exclusions, and repository/branch scopes in a single run.
func TestCloneRepo_GroupsSharedHashes(t *testing.T) {
	t.Parallel()
	db, repo := openCloneTestDB(t)
	ctx := context.Background()

	seedHashedNode(t, db, hashedNode{"r1", "main", "fnA1", "function", "hashA"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "fnA2", "function", "hashA"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "fnB", "function", "hashB"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "chunk1", "chunk", "hashC"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "chunk2", "chunk", "hashC"})
	seedHashedNode(t, db, hashedNode{"r1", "feature", "fnA3", "function", "hashA"})
	seedHashedNode(t, db, hashedNode{"r2", "main", "fnA4", "function", "hashA"})

	got, err := repo.ClonedNodes(ctx, duplicates.CloneQuery{RepoID: "r1", Branch: "main"}, duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("ClonedNodes: %v", err)
	}

	ids := make([]string, 0, len(got))
	for _, n := range got {
		if n.ContentHash != "hashA" {
			t.Errorf("unexpected hash %q in results (only hashA should group)", n.ContentHash)
		}
		ids = append(ids, n.NodeID)
	}
	sort.Strings(ids)
	want := []string{"fnA1", "fnA2"}
	if len(ids) != len(want) {
		t.Fatalf("got members %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got members %v, want %v", ids, want)
		}
	}
}

// TestCloneRepo_EmptyHashesDoNotGroup pins empty-hash clone grouping: nodes whose
// content_hash is ” (the index path leaves it empty when no hash is computed)
// must NOT be bucketed together as byte-identical clones. A genuine hashed pair
// alongside proves real grouping still works.
func TestCloneRepo_EmptyHashesDoNotGroup(t *testing.T) {
	t.Parallel()
	db, repo := openCloneTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"e1", "e2", "e3", "e4"} {
		seedHashedNode(t, db, hashedNode{"r1", "main", id, "function", ""})
	}
	seedHashedNode(t, db, hashedNode{"r1", "main", "x1", "function", "hx"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "x2", "function", "hx"})

	got, err := repo.ClonedNodes(ctx, duplicates.CloneQuery{RepoID: "r1", Branch: "main"}, duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("ClonedNodes: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, n := range got {
		if n.ContentHash == "" {
			t.Errorf("empty-hash node %q grouped as a clone (false byte-identity)", n.NodeID)
		}
		ids = append(ids, n.NodeID)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "x1" || ids[1] != "x2" {
		t.Fatalf("got %v, want only the genuine pair [x1 x2]", ids)
	}
}

// TestCloneRepo_KindCountIsolation verifies that a chunk sharing a hash with a function does not trigger grouping when the chunk kind is excluded.
func TestCloneRepo_KindCountIsolation(t *testing.T) {
	t.Parallel()
	db, repo := openCloneTestDB(t)
	ctx := context.Background()

	seedHashedNode(t, db, hashedNode{"r1", "main", "fnX", "function", "hashX"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "chunkX", "chunk", "hashX"})

	got, err := repo.ClonedNodes(ctx, duplicates.CloneQuery{RepoID: "r1", Branch: "main"}, duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("ClonedNodes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no clone group (function alone among eligible kinds), got %d rows", len(got))
	}
}

// TestCloneRepo_SimilarEdges_ThresholdScopeAndNulls verifies near-duplicate edge queries, including score thresholds, NULL exclusions, and branch scopes.
func TestCloneRepo_SimilarEdges_ThresholdScopeAndNulls(t *testing.T) {
	t.Parallel()
	db, repo := openCloneTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c", "ch"} {
		kind := "function"
		if id == "ch" {
			kind = "chunk"
		}
		seedHashedNode(t, db, hashedNode{"r1", "main", id, kind, "h-" + id})
	}
	seedHashedNode(t, db, hashedNode{"r1", "feature", "fa", "function", "h-fa"})
	seedHashedNode(t, db, hashedNode{"r1", "feature", "fb", "function", "h-fb"})

	seedSimilarEdge(t, db, simEdge{"r1", "main", "a", "b", 0.90})
	seedSimilarEdge(t, db, simEdge{"r1", "main", "a", "c", 0.50})
	seedSimilarEdge(t, db, simEdge{"r1", "main", "b", "c", nil})
	seedSimilarEdge(t, db, simEdge{"r1", "main", "a", "ch", 0.99})
	seedSimilarEdge(t, db, simEdge{"r1", "feature", "fa", "fb", 0.99})

	got, err := repo.SimilarEdges(ctx, "r1", "main", 0.80, duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("SimilarEdges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 edge (a~b @0.90), got %d: %+v", len(got), got)
	}
	e := got[0]
	if e.Score != 0.90 {
		t.Errorf("score = %v, want 0.90", e.Score)
	}
	if e.Src.NodeID != "a" || e.Dst.NodeID != "b" {
		t.Errorf("endpoints = (%s,%s), want (a,b)", e.Src.NodeID, e.Dst.NodeID)
	}
	if e.Src.FilePath != "a.go" || e.Dst.Kind != "function" {
		t.Errorf("metadata not hydrated: %+v", e)
	}
}
