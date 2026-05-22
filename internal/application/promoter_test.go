package application_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// newTestPromoter wires a Promoter to a real sqlite.PromotionStore over the
// given test DB, with the production FTS + embedding-ref sinks registered.
func newTestPromoter(sa *application.StagingArea, db *sql.DB) *application.Promoter {
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	return application.NewPromoter(sa, store)
}

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
    signature      TEXT,
    snippet        TEXT,
    prev_signature TEXT,
    exported       INTEGER,
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

CREATE TABLE edges (
    edge_id          TEXT NOT NULL,
    branch           TEXT NOT NULL,
    repo_id          TEXT NOT NULL,
    src_node_id      TEXT NOT NULL,
    dst_node_id      TEXT NOT NULL,
    kind             TEXT NOT NULL,
    confidence       TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (edge_id, branch),
    FOREIGN KEY (src_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE,
    FOREIGN KEY (dst_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE
);

CREATE TABLE node_embeddings (
    content_hash  TEXT PRIMARY KEY,
    model         TEXT NOT NULL,
    dim           INTEGER NOT NULL,
    embedding     BLOB NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE node_embedding_refs (
    node_id       TEXT PRIMARY KEY,
    content_hash  TEXT,
    state         TEXT NOT NULL,
    enqueued_at   INTEGER NOT NULL,
    embedded_at   INTEGER,
    FOREIGN KEY (content_hash) REFERENCES node_embeddings(content_hash)
);
CREATE INDEX idx_node_embedding_refs_state ON node_embedding_refs(state, enqueued_at);

CREATE VIRTUAL TABLE node_fts_words USING fts5(
    node_id UNINDEXED, branch UNINDEXED, repo_id UNINDEXED, words,
    tokenize = "unicode61 remove_diacritics 2"
);
CREATE VIRTUAL TABLE node_fts_trigrams USING fts5(
    node_id UNINDEXED, branch UNINDEXED, repo_id UNINDEXED, raw,
    tokenize = "trigram"
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
// correct node rows and queue rows (3 work_kinds × 2 files + 1 per-promotion
// wiki row = 7 queue rows).
func TestPromote_TwoFiles(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := application.NewStagingArea()
	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	n2, _ := domain.NewNode("n2", "b.go", "B", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1}, nil)
	sa.StageFile("repo1", "main", "b.go", []*domain.Node{n2}, nil)

	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-abc", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got := countNodes(t, db); got != 2 {
		t.Errorf("nodes: want 2, got %d", got)
	}
	if got := countQueue(t, db); got != 7 {
		t.Errorf("queue rows: want 7 (3 work_kinds × 2 files + 1 wiki), got %d", got)
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

	sa := application.NewStagingArea()
	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-abc", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
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

	sa := application.NewStagingArea()

	// First promote.
	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1}, nil)
	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-001", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	// Second promote with the same node.
	n1b, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1b}, nil)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-002", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("second Promote: %v", err)
	}

	// Nodes table must have exactly 1 row (re-inserted, not duplicated).
	if got := countNodes(t, db); got != 1 {
		t.Errorf("nodes after idempotent promote: want 1, got %d", got)
	}
	// Queue must have (3 work_kinds + 1 wiki) rows per promote call = 8 total.
	if got := countQueue(t, db); got != 8 {
		t.Errorf("queue rows after 2 promotes: want 8, got %d", got)
	}
}

// TestPromote_AdvancesLastPromotedSHA verifies that a successful Promote writes
// repos.last_promoted_sha and repos.active_branch atomically with the node
// rows — the contract that StartupResync's cheap-path check depends on
// (solov2-c47). The first promote stamps the SHA; a second promote with a
// different SHA overwrites it; an empty-batch (no-op) promote leaves it
// unchanged; a promote with an empty SHA does NOT clobber a known-good value.
func TestPromote_AdvancesLastPromotedSHA(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := application.NewStagingArea()
	p := newTestPromoter(sa, db)
	actor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}

	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1}, nil)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-001", actor); err != nil {
		t.Fatalf("first Promote: %v", err)
	}
	if sha, br := readRepoSHA(t, db, "repo1"); sha != "sha-001" || br != "main" {
		t.Fatalf("after first promote: sha=%q branch=%q, want sha-001/main", sha, br)
	}

	// Second promote on a different branch+sha overwrites both columns.
	n2, _ := domain.NewNode("n2", "b.go", "B", domain.KindFunction)
	sa.StageFile("repo1", "topic", "b.go", []*domain.Node{n2}, nil)
	if err := p.Promote(context.Background(), "repo1", "topic", "sha-002", actor); err != nil {
		t.Fatalf("second Promote: %v", err)
	}
	if sha, br := readRepoSHA(t, db, "repo1"); sha != "sha-002" || br != "topic" {
		t.Fatalf("after second promote: sha=%q branch=%q, want sha-002/topic", sha, br)
	}

	// Empty-batch promote (registered repo, nothing staged) returns nil and
	// must not touch the row — the early-return guards before BEGIN TX.
	if err := p.Promote(context.Background(), "repo1", "topic", "sha-noop", actor); err != nil {
		t.Fatalf("empty-batch Promote: %v", err)
	}
	if sha, br := readRepoSHA(t, db, "repo1"); sha != "sha-002" || br != "topic" {
		t.Errorf("empty-batch promote clobbered repo row: sha=%q branch=%q", sha, br)
	}

	// Defensive: a promote with an empty SHA must NOT clobber the stored value
	// (caller-error guard inside the transaction body).
	n3, _ := domain.NewNode("n3", "c.go", "C", domain.KindFunction)
	sa.StageFile("repo1", "topic", "c.go", []*domain.Node{n3}, nil)
	if err := p.Promote(context.Background(), "repo1", "topic", "", actor); err != nil {
		t.Fatalf("empty-sha Promote: %v", err)
	}
	if sha, _ := readRepoSHA(t, db, "repo1"); sha != "sha-002" {
		t.Errorf("empty-sha promote clobbered last_promoted_sha: got %q, want sha-002", sha)
	}

	// Production case: cold-scan reparser on a freshly repo.Add-ed repo
	// passes branch="" (active_branch is NULL after repo.Add). The SHA must
	// still advance so the next startup takes the cheap path; active_branch
	// is left untouched.
	insertTestRepo(t, db, "repo2")
	n4, _ := domain.NewNode("n4", "d.go", "D", domain.KindFunction)
	sa.StageFile("repo2", "", "d.go", []*domain.Node{n4}, nil)
	if err := p.Promote(context.Background(), "repo2", "", "sha-emptybr", actor); err != nil {
		t.Fatalf("empty-branch Promote: %v", err)
	}
	if sha, br := readRepoSHA(t, db, "repo2"); sha != "sha-emptybr" || br != "" {
		t.Errorf("empty-branch promote: sha=%q branch=%q, want sha-emptybr/empty", sha, br)
	}
}

// readRepoSHA returns (last_promoted_sha, active_branch) for repoID, with
// NULL flattened to "". Used by SHA-advance tests.
func readRepoSHA(t *testing.T, db *sql.DB, repoID string) (sha, branch string) {
	t.Helper()
	var s, b sql.NullString
	if err := db.QueryRow(
		`SELECT last_promoted_sha, active_branch FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&s, &b); err != nil {
		t.Fatalf("readRepoSHA: %v", err)
	}
	return s.String, b.String
}

// TestPromoteUnregisteredRepo verifies that Promote returns application.ErrUnregisteredRepo
// when the repoID is not present in the repos table.
func TestPromoteUnregisteredRepo(t *testing.T) {
	db := openMemDB(t)
	// Intentionally do NOT insert a repos row.

	sa := application.NewStagingArea()
	p := newTestPromoter(sa, db)

	err := p.Promote(context.Background(), "unknown-repo", "main", "sha-abc", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem})
	if err == nil {
		t.Fatal("expected application.ErrUnregisteredRepo, got nil")
		return
	}
	var unreg application.ErrUnregisteredRepo
	if !errors.As(err, &unreg) {
		t.Fatalf("expected application.ErrUnregisteredRepo, got %T: %v", err, err)
	}
	if unreg.RepoID != "unknown-repo" {
		t.Errorf("application.ErrUnregisteredRepo.RepoID: want %q, got %q", "unknown-repo", unreg.RepoID)
	}
	if !strings.Contains(err.Error(), "veska repo add") {
		t.Errorf("error message should contain 'veska repo add', got: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown-repo") {
		t.Errorf("error message should contain repoID, got: %q", err.Error())
	}
}

// TestPromoteRegisteredRepo verifies that Promote proceeds normally (no error)
// when the repoID exists in the repos table, even with empty staging.
func TestPromoteRegisteredRepo(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "known-repo")

	sa := application.NewStagingArea()
	p := newTestPromoter(sa, db)

	// Empty staging — should return nil (not application.ErrUnregisteredRepo).
	if err := p.Promote(context.Background(), "known-repo", "main", "sha-abc", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("expected no error for registered repo, got: %v", err)
	}
}

// TestPromote_WritesFTS verifies that promoting a node lands rows in
// both node_fts_words and node_fts_trigrams within the same transaction,
// using the camelCase-split pre-tokenisation contract from m3.03.2.
func TestPromote_WritesFTS(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo-fts")

	sa := application.NewStagingArea()
	// Mirror the DoD example: kind=function, symbol path (n.Name) =
	// "pkg/api/closeFinding". n.Path (file_path) is irrelevant here.
	n, _ := domain.NewNode("n1", "src/api.go", "pkg/api/closeFinding", domain.KindFunction)
	sa.StageFile("repo-fts", "main", "src/api.go", []*domain.Node{n}, nil)

	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo-fts", "main", "sha", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var c int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_fts_words WHERE node_id=?`, "n1").Scan(&c); err != nil {
		t.Fatalf("count words: %v", err)
	}
	if c != 1 {
		t.Errorf("node_fts_words rows for n1: want 1, got %d", c)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_fts_trigrams WHERE node_id=?`, "n1").Scan(&c); err != nil {
		t.Fatalf("count trigrams: %v", err)
	}
	if c != 1 {
		t.Errorf("node_fts_trigrams rows for n1: want 1, got %d", c)
	}

	// words must contain the camelCase splits.
	var words string
	if err := db.QueryRow(`SELECT words FROM node_fts_words WHERE node_id=?`, "n1").Scan(&words); err != nil {
		t.Fatalf("select words: %v", err)
	}
	for _, tok := range []string{"function", "pkg", "api", "closeFinding", "close", "Finding"} {
		if !strings.Contains(words, tok) {
			t.Errorf("words column %q missing token %q", words, tok)
		}
	}

	// trigram MATCH on a substring inside the camelCased symbol.
	var got string
	if err := db.QueryRow(
		`SELECT node_id FROM node_fts_trigrams WHERE raw MATCH ?`,
		"ind",
	).Scan(&got); err != nil {
		t.Fatalf("trigram MATCH: %v", err)
	}
	if got != "n1" {
		t.Errorf("trigram match returned %q, want n1", got)
	}
}

// TestPromote_FTS_RemovesStaleRowsOnReParse verifies that when a file is
// re-promoted with a smaller node set, the FTS rows for nodes that
// disappear are also cleared.
func TestPromote_FTS_RemovesStaleRowsOnReParse(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo-fts")

	sa := application.NewStagingArea()
	a, _ := domain.NewNode("a", "f.go", "closeFinding", domain.KindFunction)
	b, _ := domain.NewNode("b", "f.go", "openFinding", domain.KindFunction)
	sa.StageFile("repo-fts", "main", "f.go", []*domain.Node{a, b}, nil)

	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo-fts", "main", "sha-1", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote 1: %v", err)
	}

	// Re-promote with only one of the two nodes.
	a2, _ := domain.NewNode("a", "f.go", "closeFinding", domain.KindFunction)
	sa.StageFile("repo-fts", "main", "f.go", []*domain.Node{a2}, nil)
	if err := p.Promote(context.Background(), "repo-fts", "main", "sha-2", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote 2: %v", err)
	}

	var c int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_fts_words`).Scan(&c); err != nil {
		t.Fatalf("count words: %v", err)
	}
	if c != 1 {
		t.Errorf("node_fts_words after re-parse: want 1 (b removed), got %d", c)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_fts_trigrams`).Scan(&c); err != nil {
		t.Fatalf("count trigrams: %v", err)
	}
	if c != 1 {
		t.Errorf("node_fts_trigrams after re-parse: want 1 (b removed), got %d", c)
	}
}

// TestPromote_AtomicTransaction verifies that a single Promote call writes all
// nodes and queue rows in one transaction. We verify by checking row counts are
// atomically consistent (no partial state visible).
func TestPromote_AtomicTransaction(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := application.NewStagingArea()
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

	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-tx", domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// 5 nodes, 3 work_kinds × 1 file + 1 wiki = 4 queue rows.
	if got := countNodes(t, db); got != 5 {
		t.Errorf("nodes: want 5, got %d", got)
	}
	if got := countQueue(t, db); got != 4 {
		t.Errorf("queue rows: want 4 (1 file × 3 work_kinds + 1 wiki), got %d", got)
	}
}
