package embedder_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	infsqlite "github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/observability"
)

// openSchemaDB returns an in-memory SQLite DB with the columns the worker
// needs from migration 0001 + 0004.
func openSchemaDB(t *testing.T) *sql.DB {
	t.Helper()
	// Each test gets its own private in-memory DB (no cache=shared) so
	// parallel/sequential tests don't see each other's rows. Using a
	// random name in case modernc.org/sqlite ever changes the default.
	dsn := "file:" + t.Name() + "?mode=memory&_foreign_keys=on"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
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
);
CREATE INDEX idx_node_embedding_refs_state ON node_embedding_refs(state, enqueued_at);
`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func seedNode(t *testing.T, db *sql.DB, nodeID, repo, branch, symbol, kind string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO nodes (node_id, branch, repo_id, language, kind, symbol_path, file_path, content_hash, last_promoted_at, actor_id, actor_kind) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, branch, repo, "go", kind, symbol, "f.go", "ch", 1, "test", "system")
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	_, err = db.Exec(`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
		nodeID, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("seed ref: %v", err)
	}
}

// fakeEmbedder returns deterministic vectors and (optionally) errors for
// specified texts.
type fakeEmbedder struct {
	mu      sync.Mutex
	calls   atomic.Int64
	errOn   map[string]error
	vector  []float32
	modelID string
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errOn[text]; ok {
		return nil, err
	}
	// Return a copy with a per-input perturbation so different texts
	// produce different vectors (and therefore different content_hashes).
	// Identical inputs yield identical vectors — used by the dedup test.
	out := make([]float32, len(f.vector))
	copy(out, f.vector)
	if len(text) > 0 && len(out) > 0 {
		// Use the last rune so identical texts still match (dedup test)
		// but the test cases with differing trailing chars produce
		// distinct float values without losing precision through int.
		runes := []rune(text)
		out[0] = float32(runes[len(runes)-1])
	}
	return out, nil
}

func (f *fakeEmbedder) ModelID() string { return f.modelID }

// fakeVectorStore records every UpsertEmbeddings call.
type fakeVectorStore struct {
	mu      sync.Mutex
	batches []vecCall
}

type vecCall struct {
	repo   string
	branch string
	rows   []domain.EmbeddingRow
}

func (s *fakeVectorStore) UpsertEmbeddings(_ context.Context, repo, branch string, rows []domain.EmbeddingRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]domain.EmbeddingRow, len(rows))
	copy(cp, rows)
	s.batches = append(s.batches, vecCall{repo: repo, branch: branch, rows: cp})
	return nil
}

func (s *fakeVectorStore) Search(context.Context, string, string, []float32, int, domain.Filter) ([]domain.Hit, error) {
	return nil, nil
}

func (s *fakeVectorStore) Reindex(context.Context, string, string) error { return nil }

func (s *fakeVectorStore) LookupContentHashes(context.Context, string, string, []string) (map[string]string, error) {
	return nil, nil
}

func (s *fakeVectorStore) snapshot() []vecCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]vecCall, len(s.batches))
	copy(out, s.batches)
	return out
}

// waitForCondition polls cond every 10ms until it returns true or the
// timeout expires. Returns true if cond succeeded.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestWorker_DrainsPendingAndMarksReady(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "n1", "r1", "main", "pkg.A", "function")
	seedNode(t, db, "n2", "r1", "main", "pkg.B", "function")

	emb := &fakeEmbedder{vector: []float32{0.1, 0.2, 0.3}, modelID: "test-model"}
	vs := &fakeVectorStore{}

	w := embedder.NewWorker(repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	ok := waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 2
	})
	cancel()
	w.Wait()

	if !ok {
		t.Fatalf("expected 2 refs ready, embed calls=%d", emb.calls.Load())
	}

	// node_embeddings has 2 rows (different texts → different vectors → different hashes).
	var nodeEmbCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&nodeEmbCount)
	if nodeEmbCount != 2 {
		t.Errorf("node_embeddings rows: want 2, got %d", nodeEmbCount)
	}

	// Each ready ref must have a non-NULL content_hash and embedded_at.
	rows, _ := db.Query(`SELECT node_id, content_hash, embedded_at FROM node_embedding_refs WHERE state='ready'`)
	defer rows.Close()
	for rows.Next() {
		var nid string
		var ch, ea sql.NullString
		_ = rows.Scan(&nid, &ch, &ea)
		if !ch.Valid || ch.String == "" {
			t.Errorf("node %s: missing content_hash", nid)
		}
		if !ea.Valid {
			t.Errorf("node %s: missing embedded_at", nid)
		}
	}

	// VectorStorage saw at least one batch covering both nodes.
	saw := map[string]bool{}
	for _, c := range vs.snapshot() {
		if c.repo != "r1" || c.branch != "main" {
			t.Errorf("vec batch wrong key: %+v", c)
		}
		for _, r := range c.rows {
			saw[r.NodeID] = true
			if r.ModelID != "test-model" {
				t.Errorf("vec row %s: model want test-model, got %q", r.NodeID, r.ModelID)
			}
			if len(r.Vector) != 3 {
				t.Errorf("vec row %s: dim want 3, got %d", r.NodeID, len(r.Vector))
			}
		}
	}
	if !saw["n1"] || !saw["n2"] {
		t.Errorf("vector store missing nodes: %v", saw)
	}
}

func TestWorker_PerRowEmbedErrorKeepsSiblingsSucceeding(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "good", "r1", "main", "pkg.G", "function")
	seedNode(t, db, "bad", "r1", "main", "pkg.B", "function")

	emb := &fakeEmbedder{
		vector:  []float32{1, 2, 3},
		modelID: "m",
		errOn:   map[string]error{"function pkg.B": errors.New("boom")},
	}
	vs := &fakeVectorStore{}

	w := embedder.NewWorker(repo, emb, vs, embedder.WithInterval(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	ok := waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 1
	})
	cancel()
	w.Wait()

	if !ok {
		t.Fatal("expected 1 ready row")
	}

	var goodState, badState string
	_ = db.QueryRow(`SELECT state FROM node_embedding_refs WHERE node_id='good'`).Scan(&goodState)
	_ = db.QueryRow(`SELECT state FROM node_embedding_refs WHERE node_id='bad'`).Scan(&badState)
	if goodState != "ready" {
		t.Errorf("good: want ready, got %q", goodState)
	}
	if badState != "pending" {
		t.Errorf("bad: want pending (retry policy is m3.02.3), got %q", badState)
	}
}

func TestWorker_IdempotentSameContentHash(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	// Two nodes whose Text projections will collide — same kind, same symbol.
	seedNode(t, db, "a", "r1", "main", "X", "function")
	seedNode(t, db, "b", "r1", "main", "X", "function")
	// Make Embed return identical vectors regardless of input length by
	// pre-padding the input so out[0]=len(text) collides.
	emb := &fakeEmbedder{vector: []float32{9, 9, 9}, modelID: "m"}
	// Patch: make vector independent of text length by zeroing out[0].
	emb.vector = []float32{0, 0, 0}
	emb.modelID = "m-stable"

	vs := &fakeVectorStore{}
	w := embedder.NewWorker(repo, emb, vs, embedder.WithInterval(5*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	ok := waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 2
	})
	cancel()
	w.Wait()

	if !ok {
		t.Fatal("expected 2 ready")
	}

	// Because both nodes hashed to the same content_hash, node_embeddings
	// has exactly 1 row.
	// We need to bypass the per-text out[0] = len(text) injection above.
	// In the fake, Embed sets out[0]=len(text). Both nodes' Text is
	// "function X" (10 bytes) — same length — so vectors collide.
	var nec int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&nec)
	if nec != 1 {
		t.Errorf("node_embeddings rows: want 1 (dedup by content_hash), got %d", nec)
	}
}

func TestWorker_CtxCancelStopsCleanly(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	emb := &fakeEmbedder{vector: []float32{1}, modelID: "m"}
	vs := &fakeVectorStore{}
	w := embedder.NewWorker(repo, emb, vs, embedder.WithInterval(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// Let it tick at least once.
	time.Sleep(20 * time.Millisecond)
	cancel()

	exited := make(chan struct{})
	go func() {
		w.Wait()
		close(exited)
	}()

	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit within 1s of ctx cancel")
	}
}

func TestWorker_StopIsIdempotent(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	w := embedder.NewWorker(repo,
		&fakeEmbedder{vector: []float32{1}, modelID: "m"},
		&fakeVectorStore{},
		embedder.WithInterval(5*time.Millisecond))

	w.Start(context.Background())
	w.Stop()
	w.Stop() // second call must not panic or block
}

func TestWorker_GaugeTracksPending(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "n1", "r1", "main", "S", "function")
	seedNode(t, db, "n2", "r1", "main", "S2", "function")

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	// Block Embed so the worker observes the gauge at depth=2 first.
	block := make(chan struct{})
	emb := &blockingEmbedder{block: block, modelID: "m", vector: []float32{1, 2}}
	vs := &fakeVectorStore{}

	w := embedder.NewWorker(repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMetrics(metrics),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	if !waitForCondition(t, 2*time.Second, func() bool {
		return readGauge(metrics.EmbedQueueDepth) == 2
	}) {
		t.Fatalf("gauge never reached 2; current=%v", readGauge(metrics.EmbedQueueDepth))
	}

	// Unblock and let it drain.
	close(block)

	if !waitForCondition(t, 2*time.Second, func() bool {
		return readGauge(metrics.EmbedQueueDepth) == 0
	}) {
		t.Fatalf("gauge never drained to 0; current=%v", readGauge(metrics.EmbedQueueDepth))
	}

	cancel()
	w.Wait()
}

type blockingEmbedder struct {
	block   chan struct{}
	modelID string
	vector  []float32
	once    sync.Once
}

func (b *blockingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	b.once.Do(func() { <-b.block })
	out := make([]float32, len(b.vector))
	copy(out, b.vector)
	return out, nil
}

func (b *blockingEmbedder) ModelID() string { return b.modelID }

// readGauge returns the current value of a prometheus.Gauge.
func readGauge(g prometheus.Gauge) float64 {
	return testutil.ToFloat64(g)
}

// Compile-time check our embedder satisfies the port.
var _ ports.EmbeddingProvider = (*fakeEmbedder)(nil)
var _ ports.EmbeddingProvider = (*blockingEmbedder)(nil)
var _ ports.VectorStorage = (*fakeVectorStore)(nil)
