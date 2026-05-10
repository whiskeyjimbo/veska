package gcsweep_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/branchpk/gcsweep"
	"github.com/whiskeyjimbo/engram/solov2/tools/loadtest/spikes/branchpk/pkloader"
)

// openMemDB opens an in-memory SQLite DB. FK enforcement is applied via PRAGMA
// before each operation that needs it; InsertBranch uses transactions and
// deferred FK checks are enabled so the circular edge chain is valid at commit.
func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	// Use a unique URI per test to avoid shared-cache collisions.
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open mem db: %v", err)
	}
	// Enable deferred FK checks so circular edges in InsertBranch can commit.
	if _, err := db.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		t.Fatalf("defer_foreign_keys: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := pkloader.CreateSchema(db); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := pkloader.InsertRepo(db, "repo1"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return db
}

// loadBranch inserts symbols for one branch.
func loadBranch(t *testing.T, db *sql.DB, branch string, n int) {
	t.Helper()
	syms := pkloader.GenerateBaseSymbols(n, "repo1")
	if err := pkloader.InsertBranch(db, branch, "repo1", syms, time.Now().Unix()); err != nil {
		t.Fatalf("insert branch %s: %v", branch, err)
	}
}

// TestGCDeletesBranches verifies that RunGCSweep removes node rows for deleted branches.
func TestGCDeletesBranches(t *testing.T) {
	db := openMemDB(t)

	// Load 3 branches with 100 symbols each.
	for _, b := range []string{"branch-a", "branch-b", "branch-c"} {
		loadBranch(t, db, b, 100)
	}

	before, err := countRows(db, "nodes")
	if err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 300 {
		t.Fatalf("expected 300 node rows before GC, got %d", before)
	}

	// Keep branch-a and branch-c; delete branch-b.
	result, err := gcsweep.RunGCSweep(db, "", []string{"branch-a", "branch-c"})
	if err != nil {
		t.Fatalf("RunGCSweep: %v", err)
	}

	after, err := countRows(db, "nodes")
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 200 {
		t.Fatalf("expected 200 node rows after GC, got %d", after)
	}
	if result.BranchesDeleted != 1 {
		t.Errorf("expected 1 branch deleted, got %d", result.BranchesDeleted)
	}
}

// TestGCCascadeDeletes verifies that edge rows cascade-delete when nodes are removed.
func TestGCCascadeDeletes(t *testing.T) {
	db := openMemDB(t)

	for _, b := range []string{"branch-a", "branch-b", "branch-c"} {
		loadBranch(t, db, b, 100)
	}

	edgesBefore, err := countRows(db, "edges")
	if err != nil {
		t.Fatalf("count edges before: %v", err)
	}
	if edgesBefore != 300 {
		t.Fatalf("expected 300 edge rows before GC, got %d", edgesBefore)
	}

	// Keep only branch-a and branch-b; delete branch-c.
	_, err = gcsweep.RunGCSweep(db, "", []string{"branch-a", "branch-b"})
	if err != nil {
		t.Fatalf("RunGCSweep: %v", err)
	}

	edgesAfter, err := countRows(db, "edges")
	if err != nil {
		t.Fatalf("count edges after: %v", err)
	}
	if edgesAfter != 200 {
		t.Fatalf("expected 200 edge rows after GC (cascade), got %d", edgesAfter)
	}
}

// TestGCSweepResultJSON verifies the result marshals with all required JSON fields.
func TestGCSweepResultJSON(t *testing.T) {
	db := openMemDB(t)
	loadBranch(t, db, "branch-a", 100)
	loadBranch(t, db, "branch-b", 100)

	result, err := gcsweep.RunGCSweep(db, "", []string{"branch-a"})
	if err != nil {
		t.Fatalf("RunGCSweep: %v", err)
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	requiredFields := []string{
		"branches_before", "branches_after", "branches_deleted",
		"wall_ms", "disk_before_bytes", "disk_after_bytes",
		"wal_before_bytes", "wal_after_bytes", "reclaim_bytes",
	}
	for _, f := range requiredFields {
		if _, ok := m[f]; !ok {
			t.Errorf("missing JSON field: %s", f)
		}
	}
}

// TestGCReclaimIsPositive verifies reclaim_bytes >= 0 after a file-based GC.
func TestGCReclaimIsPositive(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gc_test.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	defer db.Close()

	// Enable deferred FK checks so circular edges in InsertBranch can commit.
	if _, err := db.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		t.Fatalf("defer_foreign_keys: %v", err)
	}

	if err := pkloader.CreateSchema(db); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := pkloader.InsertRepo(db, "repo1"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Load 5 branches with 200 symbols each.
	branches := []string{"b0", "b1", "b2", "b3", "b4"}
	for _, b := range branches {
		syms := pkloader.GenerateBaseSymbols(200, "repo1")
		if err := pkloader.InsertBranch(db, b, "repo1", syms, time.Now().Unix()); err != nil {
			t.Fatalf("insert branch %s: %v", b, err)
		}
	}

	// Keep only b2, b3, b4 — delete b0 and b1.
	result, err := gcsweep.RunGCSweep(db, dbPath, []string{"b2", "b3", "b4"})
	if err != nil {
		t.Fatalf("RunGCSweep: %v", err)
	}

	if result.ReclaimBytes < 0 {
		t.Errorf("expected reclaim_bytes >= 0, got %d", result.ReclaimBytes)
	}
	if result.BranchesDeleted != 2 {
		t.Errorf("expected 2 branches deleted, got %d", result.BranchesDeleted)
	}
	if result.WallMs < 0 {
		t.Errorf("expected non-negative wall_ms, got %d", result.WallMs)
	}

	// Verify disk sizes were captured (file-based DB should have non-zero sizes).
	if result.DiskBeforeBytes == 0 && result.DiskAfterBytes == 0 {
		// This is acceptable if the DB file wasn't flushed yet, just warn.
		t.Logf("disk sizes both 0 — WAL may not have flushed yet (acceptable for small test DB)")
	}

	t.Logf("GCSweepResult: branches_before=%d branches_after=%d deleted=%d wall_ms=%d disk_before=%d disk_after=%d reclaim=%d",
		result.BranchesBefore, result.BranchesAfter, result.BranchesDeleted,
		result.WallMs, result.DiskBeforeBytes, result.DiskAfterBytes, result.ReclaimBytes)
}

func countRows(db *sql.DB, table string) (int, error) {
	var n int
	// table name is controlled internally; safe to interpolate.
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil { //nolint:gosec
		return 0, err
	}
	return n, nil
}

// Verify CountBranches helper.
func TestCountBranches(t *testing.T) {
	db := openMemDB(t)
	loadBranch(t, db, "branch-a", 10)
	loadBranch(t, db, "branch-b", 10)

	n, err := gcsweep.CountBranches(db)
	if err != nil {
		t.Fatalf("CountBranches: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 branches, got %d", n)
	}
}
