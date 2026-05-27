package embedder_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
)

// TestRequeueAllUnderNewModel pins solov2-fz8: on embedder switch, every ready
// ref must flip back to pending AND the content-addressed embedding store must
// be wiped (otherwise the next embed of the same content is a no-op under
// ON CONFLICT(content_hash) and the old-model vector persists).
func TestRequeueAllUnderNewModel(t *testing.T) {
	t.Parallel()
	db, err := sql.Open(sqldriver.Name, "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
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
    attempts      INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (content_hash) REFERENCES node_embeddings(content_hash)
);`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO node_embeddings VALUES (?,?,?,?,?)`, "h1", "old-model", 3, []byte{0, 0, 0}, 0)
	mustExec(`INSERT INTO node_embeddings VALUES (?,?,?,?,?)`, "h2", "old-model", 3, []byte{0, 0, 0}, 0)
	mustExec(`INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at, embedded_at, attempts)
	          VALUES ('n1', 'h1', 'ready', 0, 5, 2)`)
	mustExec(`INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at, embedded_at, attempts)
	          VALUES ('n2', 'h2', 'failed', 0, NULL, 9)`)

	n, err := embedder.RequeueAllUnderNewModel(context.Background(), db)
	if err != nil {
		t.Fatalf("RequeueAllUnderNewModel: %v", err)
	}
	if n != 2 {
		t.Errorf("rows requeued = %d, want 2", n)
	}

	var embCount int
	db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&embCount)
	if embCount != 0 {
		t.Errorf("node_embeddings not cleared: %d rows remain", embCount)
	}

	rows, _ := db.Query(`SELECT node_id, content_hash, state, embedded_at, attempts FROM node_embedding_refs ORDER BY node_id`)
	defer rows.Close()
	for rows.Next() {
		var nodeID, state string
		var ch sql.NullString
		var emb sql.NullInt64
		var attempts int
		if err := rows.Scan(&nodeID, &ch, &state, &emb, &attempts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if state != "pending" || ch.Valid || emb.Valid || attempts != 0 {
			t.Errorf("ref %s: state=%q content_hash=%v embedded_at=%v attempts=%d, want pending/NULL/NULL/0",
				nodeID, state, ch, emb, attempts)
		}
	}
}
