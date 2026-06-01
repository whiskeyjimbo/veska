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

// hashedNode bundles the identifiers for one seeded node row so the helper
// stays within the 5-arg budget.
type hashedNode struct {
	repoID, branch, nodeID, kind, contentHash string
}

// seedHashedNode inserts a node row with an explicit content_hash and kind so
// clone-grouping behaviour can be exercised directly.
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

// simEdge bundles the identifiers for one seeded SIMILAR_TO edge so the helper
// stays within the 5-arg budget. A nil score inserts NULL (legacy edge).
type simEdge struct {
	repoID, branch, src, dst string
	score                    any
}

// seedSimilarEdge inserts a SIMILAR_TO edge with a score between two existing
// nodes.
func seedSimilarEdge(t *testing.T, db *sql.DB, e simEdge) {
	t.Helper()
	now := time.Now().UnixMilli()
	edgeID := e.src + "->" + e.dst // test-local synthetic id; uniqueness is all that matters
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

// TestCloneRepo_GroupsSharedHashes verifies the >=2 grouping, the kind
// exclusion, and the repo/branch scoping in one pass.
func TestCloneRepo_GroupsSharedHashes(t *testing.T) {
	t.Parallel()
	db, repo := openCloneTestDB(t)
	ctx := context.Background()

	// Two functions share hashA -> a real clone group.
	seedHashedNode(t, db, hashedNode{"r1", "main", "fnA1", "function", "hashA"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "fnA2", "function", "hashA"})
	// A lone function with a unique hash -> excluded (count 1).
	seedHashedNode(t, db, hashedNode{"r1", "main", "fnB", "function", "hashB"})
	// Two chunks share hashC, but chunk is an excluded kind -> not a group.
	seedHashedNode(t, db, hashedNode{"r1", "main", "chunk1", "chunk", "hashC"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "chunk2", "chunk", "hashC"})
	// A function on another branch shares hashA but must not bleed in.
	seedHashedNode(t, db, hashedNode{"r1", "feature", "fnA3", "function", "hashA"})
	// A function in another repo shares hashA but must not bleed in.
	seedHashedNode(t, db, hashedNode{"r2", "main", "fnA4", "function", "hashA"})

	got, err := repo.ClonedNodes(ctx, "r1", "main", duplicates.ExcludedKinds)
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

// TestCloneRepo_KindCountIsolation verifies a chunk sharing a hash with a
// function does not push the function group over the >=2 threshold on its own:
// the COUNT must reflect only eligible (non-excluded) kinds.
func TestCloneRepo_KindCountIsolation(t *testing.T) {
	t.Parallel()
	db, repo := openCloneTestDB(t)
	ctx := context.Background()

	// One function + one chunk share hashX. The function is alone among
	// eligible kinds, so hashX must NOT form a group.
	seedHashedNode(t, db, hashedNode{"r1", "main", "fnX", "function", "hashX"})
	seedHashedNode(t, db, hashedNode{"r1", "main", "chunkX", "chunk", "hashX"})

	got, err := repo.ClonedNodes(ctx, "r1", "main", duplicates.ExcludedKinds)
	if err != nil {
		t.Fatalf("ClonedNodes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no clone group (function alone among eligible kinds), got %d rows", len(got))
	}
}

// TestCloneRepo_SimilarEdges_ThresholdScopeAndNulls verifies the near-dup
// query: score threshold, NULL-score exclusion, kind exclusion on both
// endpoints, repo/branch scoping, and metadata hydration.
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

	seedSimilarEdge(t, db, simEdge{"r1", "main", "a", "b", 0.90})      // kept
	seedSimilarEdge(t, db, simEdge{"r1", "main", "a", "c", 0.50})      // below threshold
	seedSimilarEdge(t, db, simEdge{"r1", "main", "b", "c", nil})       // NULL score -> excluded
	seedSimilarEdge(t, db, simEdge{"r1", "main", "a", "ch", 0.99})     // chunk endpoint -> excluded
	seedSimilarEdge(t, db, simEdge{"r1", "feature", "fa", "fb", 0.99}) // other branch -> excluded

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
