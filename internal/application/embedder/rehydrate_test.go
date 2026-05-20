package embedder_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestRehydrateVectors_LoadsReadyRows: a freshly opened DB with N ready
// node_embedding_refs (each pointing at a node_embeddings row) hydrates
// the spy VectorStorage with one UpsertEmbeddings call per (repo, branch)
// bucket. Stand-in for the daemon-restart scenario that motivated solov2-249.
func TestRehydrateVectors_LoadsReadyRows(t *testing.T) {
	db := newRehydrateDB(t)

	// Two buckets: repo1/main and repo1/topic, two nodes in main, one in topic.
	insertReady(t, db, "repo1", "main", "n1", "h1", "m1", []float32{1, 0, 0})
	insertReady(t, db, "repo1", "main", "n2", "h2", "m1", []float32{0, 1, 0})
	insertReady(t, db, "repo1", "topic", "n3", "h3", "m1", []float32{0, 0, 1})

	// A 'pending' row that must NOT rehydrate (no embedding yet).
	insertNode(t, db, "repo1", "main", "n4")
	mustExec(t, db, `INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at)
                     VALUES ('n4', NULL, 'pending', 0)`)

	vec := &spyVector{}
	counts, err := embedder.RehydrateVectors(context.Background(), db, vec)
	if err != nil {
		t.Fatalf("RehydrateVectors: %v", err)
	}
	if got := counts["repo1@main"]; got != 2 {
		t.Errorf("counts[repo1@main] = %d, want 2", got)
	}
	if got := counts["repo1@topic"]; got != 1 {
		t.Errorf("counts[repo1@topic] = %d, want 1", got)
	}
	if len(vec.calls) != 2 {
		t.Fatalf("UpsertEmbeddings calls = %d, want 2; %+v", len(vec.calls), vec.calls)
	}
	for _, c := range vec.calls {
		for _, r := range c.batch {
			if r.NodeID == "n4" {
				t.Errorf("rehydrate included pending node n4")
			}
		}
	}
}

func TestRehydrateVectors_NilDeps(t *testing.T) {
	if _, err := embedder.RehydrateVectors(context.Background(), nil, &spyVector{}); !errors.Is(err, embedder.ErrMissingDependency) {
		t.Errorf("nil readDB: want ErrMissingDependency, got %v", err)
	}
	if _, err := embedder.RehydrateVectors(context.Background(), newRehydrateDB(t), nil); !errors.Is(err, embedder.ErrMissingDependency) {
		t.Errorf("nil vectors: want ErrMissingDependency, got %v", err)
	}
}

// TestRehydrateVectors_Idempotent: a second invocation produces the same
// store contents. The contract relies on VectorStorage.UpsertEmbeddings
// keying by node_id within (repo, branch), so multiple runs do not bloat
// the store — the spy here only counts call shape, not store state.
func TestRehydrateVectors_Idempotent(t *testing.T) {
	db := newRehydrateDB(t)
	insertReady(t, db, "repo1", "main", "n1", "h1", "m1", []float32{1, 0, 0})

	vec := &spyVector{}
	if _, err := embedder.RehydrateVectors(context.Background(), db, vec); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := embedder.RehydrateVectors(context.Background(), db, vec); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(vec.calls) != 2 {
		t.Errorf("two runs → want 2 Upsert calls (one per run), got %d", len(vec.calls))
	}
	for i, c := range vec.calls {
		if len(c.batch) != 1 {
			t.Errorf("run %d batch size = %d, want 1", i, len(c.batch))
		}
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

type spyCall struct {
	repo, branch string
	batch        []domain.EmbeddingRow
}

type spyVector struct{ calls []spyCall }

func (s *spyVector) UpsertEmbeddings(_ context.Context, repoID, branch string, batch []domain.EmbeddingRow) error {
	cp := make([]domain.EmbeddingRow, len(batch))
	copy(cp, batch)
	s.calls = append(s.calls, spyCall{repo: repoID, branch: branch, batch: cp})
	return nil
}

func (s *spyVector) Search(_ context.Context, _, _ string, _ []float32, _ int, _ domain.Filter) ([]domain.Hit, error) {
	return nil, nil
}

func (s *spyVector) LookupContentHashes(context.Context, string, string, []string) (map[string]string, error) {
	return nil, nil
}

func (s *spyVector) Reindex(context.Context, string, string) error { return nil }

func newRehydrateDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schema := `
CREATE TABLE nodes (
    node_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL,
    PRIMARY KEY (node_id, branch)
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
);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func insertReady(t *testing.T, db *sql.DB, repo, branch, nodeID, contentHash, model string, vec []float32) {
	t.Helper()
	insertNode(t, db, repo, branch, nodeID)
	mustExec(t, db, `
		INSERT OR IGNORE INTO node_embeddings (content_hash, model, dim, embedding, created_at)
		VALUES (?, ?, ?, ?, 0)`,
		contentHash, model, len(vec), encodeFloat32LE(vec),
	)
	mustExec(t, db, `
		INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at)
		VALUES (?, ?, 'ready', 0)`,
		nodeID, contentHash,
	)
}

func insertNode(t *testing.T, db *sql.DB, repo, branch, nodeID string) {
	t.Helper()
	mustExec(t, db, `
		INSERT INTO nodes (node_id, branch, repo_id, language, kind, symbol_path,
		                   file_path, content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?, ?, ?, '', 'function', '', '', '', 0, 'system:test', 'system')`,
		nodeID, branch, repo,
	)
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// encodeFloat32LE mirrors embedder.encodeFloat32LE — duplicated here because
// it's unexported in the production package and the test sits in
// package embedder_test.
func encodeFloat32LE(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}
