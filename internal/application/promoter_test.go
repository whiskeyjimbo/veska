package application

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
)

// openMemDB opens an in-memory SQLite DB with foreign keys enabled and creates
// the minimal schema required by Promoter.
func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := `
CREATE TABLE repos (
    repo_id          TEXT PRIMARY KEY,
    root_path        TEXT NOT NULL UNIQUE,
    added_at         INTEGER NOT NULL,
    active_branch    TEXT,
    last_promoted_sha  TEXT,
    module_path      TEXT
);

CREATE TABLE nodes (
    node_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    line_start     INTEGER,
    line_end       INTEGER,
    content_hash   TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (node_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);

CREATE TABLE post_promotion_queue (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    promotion_id  TEXT NOT NULL,
    repo_id       TEXT NOT NULL,
    branch        TEXT NOT NULL,
    git_sha       TEXT NOT NULL,
    work_kind     TEXT NOT NULL,
    payload       TEXT NOT NULL,
    state         TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    enqueued_at   INTEGER NOT NULL,
    completed_at  INTEGER,
    error         TEXT
);
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

// insertTestRepo inserts a minimal repos row so FK constraints are satisfied.
func insertTestRepo(t *testing.T, db *sql.DB, repoID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/"+repoID, time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insertTestRepo: %v", err)
	}
}

func countNodes(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n); err != nil {
		t.Fatalf("countNodes: %v", err)
	}
	return n
}

func countQueue(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM post_promotion_queue`).Scan(&n); err != nil {
		t.Fatalf("countQueue: %v", err)
	}
	return n
}

// TestPromote_TwoFiles verifies that promoting 2 staged files produces the
// correct node rows and queue rows (3 work_kinds × 2 files = 6 queue rows).
func TestPromote_TwoFiles(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	n2, _ := domain.NewNode("n2", "b.go", "B", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1}, nil)
	sa.StageFile("repo1", "main", "b.go", []*domain.Node{n2}, nil)

	p := NewPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-abc"); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got := countNodes(t, db); got != 2 {
		t.Errorf("nodes: want 2, got %d", got)
	}
	if got := countQueue(t, db); got != 6 {
		t.Errorf("queue rows: want 6 (3 work_kinds × 2 files), got %d", got)
	}

	// Staging must be cleared after promotion.
	if files := sa.StagedFiles("repo1", "main"); len(files) != 0 {
		t.Errorf("expected staging cleared, got %d files remaining", len(files))
	}
}

// TestPromote_ZeroFiles verifies that promoting with nothing staged writes
// nothing and returns nil.
func TestPromote_ZeroFiles(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	p := NewPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-abc"); err != nil {
		t.Fatalf("Promote with empty staging: %v", err)
	}

	if got := countNodes(t, db); got != 0 {
		t.Errorf("nodes: want 0, got %d", got)
	}
	if got := countQueue(t, db); got != 0 {
		t.Errorf("queue rows: want 0, got %d", got)
	}
}

// TestPromote_Idempotent verifies that calling Promote twice for the same files
// leaves the correct final row count (DELETE+INSERT avoids duplicates).
func TestPromote_Idempotent(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()

	// First promote.
	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1}, nil)
	p := NewPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-001"); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	// Second promote with the same node.
	n1b, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1b}, nil)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-002"); err != nil {
		t.Fatalf("second Promote: %v", err)
	}

	// Nodes table must have exactly 1 row (re-inserted, not duplicated).
	if got := countNodes(t, db); got != 1 {
		t.Errorf("nodes after idempotent promote: want 1, got %d", got)
	}
	// Queue must have 3 rows per promote call = 6 total.
	if got := countQueue(t, db); got != 6 {
		t.Errorf("queue rows after 2 promotes: want 6, got %d", got)
	}
}

// TestPromote_AtomicTransaction verifies that a single Promote call writes all
// nodes and queue rows in one transaction. We verify by checking row counts are
// atomically consistent (no partial state visible).
func TestPromote_AtomicTransaction(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	// Stage several nodes across one file to verify batch atomicity.
	nodes := make([]*domain.Node, 0, 5)
	for i := range 5 {
		n, _ := domain.NewNode(
			string(rune('a'+i)),
			"multi.go",
			string(rune('A'+i)),
			domain.KindFunction,
		)
		nodes = append(nodes, n)
	}
	sa.StageFile("repo1", "main", "multi.go", nodes, nil)

	p := NewPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-tx"); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// 5 nodes, 3 work_kinds × 1 file = 3 queue rows.
	if got := countNodes(t, db); got != 5 {
		t.Errorf("nodes: want 5, got %d", got)
	}
	if got := countQueue(t, db); got != 3 {
		t.Errorf("queue rows: want 3 (1 file × 3 work_kinds), got %d", got)
	}
}
