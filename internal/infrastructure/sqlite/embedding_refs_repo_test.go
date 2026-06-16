package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// seedRefRow inserts a repo, node, and pending ref so we have a row to
// mutate from MarkAttemptFailed. nodeID is unique per call.
func seedRefRow(t *testing.T, db *sql.DB, repoID, nodeID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	// Repo may already exist for prior rows in the same test — IGNORE conflict.
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, "main", repoID, "go", "function", nodeID, "f.go",
		"h", now, "test", "system"); err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
		nodeID, now); err != nil {
		t.Fatalf("insert ref %s: %v", nodeID, err)
	}
}

func openTestDB(t *testing.T) (*sql.DB, *sqlite.EmbeddingRefsRepo) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, sqlite.NewEmbeddingRefsRepo(db, db)
}

// TestEmbeddingRefsRepo_MarkAttemptFailed_BumpsAttempts verifies a single
// failure bumps attempts but does not flip state when budget remains.
func TestEmbeddingRefsRepo_MarkAttemptFailed_BumpsAttempts(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)
	seedRefRow(t, db, "r1", "n1")

	if err := repo.MarkAttemptFailed(context.Background(), "n1", 3); err != nil {
		t.Fatalf("MarkAttemptFailed: %v", err)
	}

	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state, attempts FROM node_embedding_refs WHERE node_id='n1'`).
		Scan(&state, &attempts); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "pending" {
		t.Errorf("state: want pending, got %q", state)
	}
	if attempts != 1 {
		t.Errorf("attempts: want 1, got %d", attempts)
	}
}

// TestEmbeddingRefsRepo_MarkAttemptFailed_FlipsAtBudget verifies the row
// flips to 'failed' exactly when attempts reaches maxAttempts.
func TestEmbeddingRefsRepo_MarkAttemptFailed_FlipsAtBudget(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)
	seedRefRow(t, db, "r1", "n1")

	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := repo.MarkAttemptFailed(ctx, "n1", 3); err != nil {
			t.Fatalf("MarkAttemptFailed iter %d: %v", i, err)
		}
	}

	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state, attempts FROM node_embedding_refs WHERE node_id='n1'`).
		Scan(&state, &attempts); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "failed" {
		t.Errorf("state: want failed, got %q", state)
	}
	if attempts != 3 {
		t.Errorf("attempts: want 3, got %d", attempts)
	}

	// FetchPending must exclude the failed row.
	pending, err := repo.FetchPending(ctx, 10)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("FetchPending returned failed rows: %+v", pending)
	}
}

// TestEmbeddingRefsRepo_MarkAttemptFailed_NoOpOnNonPending verifies that
// non-pending rows are not modified by MarkAttemptFailed (idempotent guard
// for race-y callers).
func TestEmbeddingRefsRepo_MarkAttemptFailed_NoOpOnNonPending(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)
	seedRefRow(t, db, "r1", "n1")

	// Force the row into 'ready' state.
	if _, err := db.Exec(`UPDATE node_embedding_refs SET state='ready' WHERE node_id='n1'`); err != nil {
		t.Fatalf("force ready: %v", err)
	}

	if err := repo.MarkAttemptFailed(context.Background(), "n1", 3); err != nil {
		t.Fatalf("MarkAttemptFailed: %v", err)
	}

	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state, attempts FROM node_embedding_refs WHERE node_id='n1'`).
		Scan(&state, &attempts); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "ready" {
		t.Errorf("state: want ready (unchanged), got %q", state)
	}
	if attempts != 0 {
		t.Errorf("attempts: want 0 (unchanged), got %d", attempts)
	}
}

// TestEmbeddingRefsRepo_CountPending_IgnoresOrphans verifies CountPending
// counts only pending refs that still have a backing node — orphaned refs
// (node deleted, ref left behind) are excluded, matching FetchPending's
// JOIN. Without this, orphans pin eng_get_status at degraded forever even
// though the worker has nothing left to drain.
func TestEmbeddingRefsRepo_CountPending_IgnoresOrphans(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)
	ctx := context.Background()

	// Two real pending refs, then orphan one by deleting its node.
	seedRefRow(t, db, "r1", "live")
	seedRefRow(t, db, "r1", "orphan")
	if _, err := db.Exec(`DELETE FROM nodes WHERE node_id='orphan'`); err != nil {
		t.Fatalf("delete node: %v", err)
	}

	// The orphaned ref row is still present and still 'pending'.
	var raw int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='pending'`).
		Scan(&raw); err != nil {
		t.Fatalf("raw count: %v", err)
	}
	if raw != 2 {
		t.Fatalf("raw pending rows: want 2 (incl orphan), got %d", raw)
	}

	// but CountPending excludes it.
	n, err := repo.CountPending(ctx)
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}
	if n != 1 {
		t.Errorf("CountPending: want 1 (orphan excluded), got %d", n)
	}

	// And FetchPending agrees — the orphan is never returned for embedding.
	pending, err := repo.FetchPending(ctx, 10)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}
	if len(pending) != 1 || pending[0].NodeID != "live" {
		t.Errorf("FetchPending: want [live], got %+v", pending)
	}
}

// TestEmbeddingRefsRepo_CountByState returns accurate counts and
// guarantees all three keys are present even when their count is zero.
func TestEmbeddingRefsRepo_CountByState(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)

	seedRefRow(t, db, "r1", "p1")
	seedRefRow(t, db, "r1", "p2")
	seedRefRow(t, db, "r1", "r1n")
	seedRefRow(t, db, "r1", "f1")

	// Flip one to ready, one to failed (without going through MarkReady which
	// requires a valid content_hash).
	if _, err := db.Exec(`UPDATE node_embedding_refs SET state='ready' WHERE node_id='r1n'`); err != nil {
		t.Fatalf("force ready: %v", err)
	}
	if _, err := db.Exec(`UPDATE node_embedding_refs SET state='failed' WHERE node_id='f1'`); err != nil {
		t.Fatalf("force failed: %v", err)
	}

	got, err := repo.CountByState(context.Background())
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	if got["pending"] != 2 {
		t.Errorf("pending: want 2, got %d", got["pending"])
	}
	if got["ready"] != 1 {
		t.Errorf("ready: want 1, got %d", got["ready"])
	}
	if got["failed"] != 1 {
		t.Errorf("failed: want 1, got %d", got["failed"])
	}
}

// TestEmbeddingRefsRepo_CountByState_AllZero verifies the map has all three
// keys present even when no rows exist.
func TestEmbeddingRefsRepo_CountByState_AllZero(t *testing.T) {
	t.Parallel()
	_, repo := openTestDB(t)

	got, err := repo.CountByState(context.Background())
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	for _, k := range []string{"pending", "ready", "failed"} {
		if v, ok := got[k]; !ok || v != 0 {
			t.Errorf("key %q: want present and 0, got ok=%v v=%d", k, ok, v)
		}
	}
}

// TestEmbeddingRefsRepo_FetchPending_TextProjection verifies the embed-input
// projection includes file_path and language so distinct nodes do not collapse
// under the content-addressed embedding dedup.
func TestEmbeddingRefsRepo_FetchPending_TextProjection(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)
	now := time.Now().UnixMilli()

	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	insertNode := func(nodeID, kind, symbol, file, lang string) {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			content_hash, last_promoted_at, actor_id, actor_kind
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			nodeID, "main", "r1", lang, kind, symbol, file,
			"h", now, "test", "system"); err != nil {
			t.Fatalf("insert node %s: %v", nodeID, err)
		}
		if _, err := db.Exec(
			`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
			nodeID, now); err != nil {
			t.Fatalf("insert ref %s: %v", nodeID, err)
		}
	}
	insertNode("n1", "function", "pkg.Foo", "a/b.go", "go")
	insertNode("n2", "function", "pkg.Foo", "c/d.ts", "typescript")
	insertNode("n3", "function", "pkg.Bar", "e.go", "")

	pending, err := repo.FetchPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}
	want := map[string]string{
		"n1": "function pkg.Foo a/b.go go",
		"n2": "function pkg.Foo c/d.ts typescript",
		"n3": "function pkg.Bar e.go", // empty language omitted
	}
	got := make(map[string]string, len(pending))
	for _, p := range pending {
		got[p.NodeID] = p.Text
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: Text = %q, want %q", id, got[id], w)
		}
	}
	// n1 and n2 share (kind, symbol) but differ in file+language — their
	// projections must differ so the embedding dedup keeps them distinct.
	if got["n1"] == got["n2"] {
		t.Error("n1 and n2 collapsed to the same projection")
	}
}

// TestEmbeddingRefsRepo_FetchPending_SnippetProjection verifies the embed-input
// projection uses EmbedVariantSnippet: a node with a persisted snippet projects
// "<kind> <symbol_path> <file> <language> <snippet>", while a node with a NULL
// snippet degrades gracefully to the exact baseline projection.
func TestEmbeddingRefsRepo_FetchPending_SnippetProjection(t *testing.T) {
	t.Parallel()
	db, repo := openTestDB(t)
	now := time.Now().UnixMilli()

	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	insertNode := func(nodeID, kind, symbol, file, lang string, snippet sql.NullString) {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			content_hash, last_promoted_at, actor_id, actor_kind, snippet
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			nodeID, "main", "r1", lang, kind, symbol, file,
			"h", now, "test", "system", snippet); err != nil {
			t.Fatalf("insert node %s: %v", nodeID, err)
		}
		if _, err := db.Exec(
			`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
			nodeID, now); err != nil {
			t.Fatalf("insert ref %s: %v", nodeID, err)
		}
	}
	insertNode("s1", "function", "pkg.Foo", "a/b.go", "go",
		sql.NullString{String: "func Foo() {}", Valid: true})
	insertNode("s2", "function", "pkg.Bar", "c.go", "go",
		sql.NullString{}) // NULL snippet

	pending, err := repo.FetchPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}
	got := make(map[string]string, len(pending))
	for _, p := range pending {
		got[p.NodeID] = p.Text
	}
	if want := "function pkg.Foo a/b.go go func Foo() {}"; got["s1"] != want {
		t.Errorf("s1: Text = %q, want %q", got["s1"], want)
	}
	if want := "function pkg.Bar c.go go"; got["s2"] != want {
		t.Errorf("s2: Text = %q, want %q (baseline)", got["s2"], want)
	}
}
