package embedder_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
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
    signature      TEXT,
    snippet        TEXT,
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
    attempts      INTEGER NOT NULL DEFAULT 0,
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

// fakeBatchEmbedder implements ports.BatchEmbeddingProvider in addition
// to the per-text Embed surface. Tracks distinct batch and single
// call counts so the test can assert which path was used (solov2-ucp).
type fakeBatchEmbedder struct {
	fakeEmbedder
	batchCalls atomic.Int64
	batchSizes []int
	mu         sync.Mutex
}

func (f *fakeBatchEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	f.batchCalls.Add(1)
	f.mu.Lock()
	f.batchSizes = append(f.batchSizes, len(texts))
	f.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := f.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

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

// mustNewWorker constructs a Worker and fails the test if the constructor
// returns an error. Used by the many happy-path tests that pass non-nil deps.
func mustNewWorker(
	t *testing.T,
	refs ports.EmbeddingRefRepo,
	emb ports.EmbeddingProvider,
	vectors ports.VectorStorage,
	opts ...embedder.Option,
) *embedder.Worker {
	t.Helper()
	w, err := embedder.NewWorker(refs, emb, vectors, opts...)
	if err != nil {
		t.Fatalf("embedder.NewWorker: %v", err)
	}
	return w
}

func TestNewWorker_NilDependencyReturnsTypedError(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)
	emb := &fakeEmbedder{}
	vs := &fakeVectorStore{}

	cases := []struct {
		name     string
		refs     ports.EmbeddingRefRepo
		embedder ports.EmbeddingProvider
		vectors  ports.VectorStorage
	}{
		{"nil refs", nil, emb, vs},
		{"nil embedder", repo, nil, vs},
		{"nil vectors", repo, emb, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := embedder.NewWorker(tc.refs, tc.embedder, tc.vectors)
			if w != nil {
				t.Errorf("expected nil *Worker, got %v", w)
			}
			if !errors.Is(err, embedder.ErrMissingDependency) {
				t.Fatalf("err = %v, want wraps ErrMissingDependency", err)
			}
		})
	}
}

func TestWorker_DrainsPendingAndMarksReady(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "n1", "r1", "main", "pkg.A", "function")
	seedNode(t, db, "n2", "r1", "main", "pkg.B", "function")

	emb := &fakeEmbedder{vector: []float32{0.1, 0.2, 0.3}, modelID: "test-model"}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
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

// TestWorker_BatchEmbedderUsedForMultipleRefs pins solov2-ucp: when
// the embedder satisfies BatchEmbeddingProvider AND a tick has > 1
// unique text to embed, the worker calls EmbedBatch once instead of
// looping Embed. The serial Embed path stays the per-row fallback.
func TestWorker_BatchEmbedderUsedForMultipleRefs(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)
	seedNode(t, db, "n1", "r1", "main", "pkg.A", "function")
	seedNode(t, db, "n2", "r1", "main", "pkg.B", "function")
	seedNode(t, db, "n3", "r1", "main", "pkg.C", "function")

	emb := &fakeBatchEmbedder{fakeEmbedder: fakeEmbedder{vector: []float32{0.1, 0.2, 0.3}, modelID: "test-model"}}
	vs := &fakeVectorStore{}
	w := mustNewWorker(t, repo, emb, vs, embedder.WithInterval(5*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	ok := waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 3
	})
	cancel()
	w.Wait()
	if !ok {
		t.Fatalf("expected 3 refs ready, embed calls=%d batch=%d", emb.calls.Load(), emb.batchCalls.Load())
	}
	if emb.batchCalls.Load() == 0 {
		t.Errorf("expected EmbedBatch to be used (got 0 batch calls, %d per-text calls)", emb.calls.Load())
	}
	// Per-text calls happen INSIDE the batch (via fakeBatchEmbedder's
	// pass-through to fakeEmbedder.Embed), so calls > 0 is expected;
	// what matters is the path was a batch.
}

// TestWorker_PauserSkipsTick pins solov2-181: when the injected pauser
// returns true, tick is a complete no-op (no FetchPending, no Embed, no
// writes). When the pauser flips to false, the worker resumes and
// drains the backlog. The contract matters because the daemon uses
// this to keep the embedder off the WriteEmbed pool while a cold-scan
// promotion is committing on WriteHot.
func TestWorker_PauserSkipsTick(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)
	seedNode(t, db, "n1", "r1", "main", "pkg.A", "function")
	seedNode(t, db, "n2", "r1", "main", "pkg.B", "function")

	emb := &fakeEmbedder{vector: []float32{0.1, 0.2, 0.3}, modelID: "test-model"}
	vs := &fakeVectorStore{}

	var paused atomic.Bool
	paused.Store(true)
	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithPauser(paused.Load),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// While paused, no Embed calls should happen even after a few ticks.
	time.Sleep(80 * time.Millisecond)
	if calls := emb.calls.Load(); calls != 0 {
		t.Errorf("paused worker still called Embed %d times", calls)
	}
	var readyWhilePaused int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&readyWhilePaused)
	if readyWhilePaused != 0 {
		t.Errorf("paused worker still drained %d refs to ready", readyWhilePaused)
	}

	// Lift the pause and the backlog should drain.
	paused.Store(false)
	ok := waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 2
	})
	cancel()
	w.Wait()
	if !ok {
		t.Fatalf("after pause lifted, expected both refs ready; embed calls=%d", emb.calls.Load())
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
		errOn:   map[string]error{"function pkg.B f.go go": errors.New("boom")},
	}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs, embedder.WithInterval(5*time.Millisecond))
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

// TestWorker_IdempotentSameContentHash is the m3.02.4 semantics check on the
// pre-existing 2.1 idempotency test: two pending refs whose nodes project to
// the same "<kind> <symbol_path>" hash to the same content_hash, so the
// second ref reuses the existing node_embeddings row WITHOUT a second Embed
// call. Both refs end up state='ready' with the same content_hash; the
// dedup-hits counter increments exactly once.
func TestWorker_IdempotentSameContentHash(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	// Two nodes whose Text projections will collide — same kind, same symbol.
	seedNode(t, db, "a", "r1", "main", "X", "function")
	seedNode(t, db, "b", "r1", "main", "X", "function")
	emb := &fakeEmbedder{vector: []float32{0, 0, 0}, modelID: "m-stable"}

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)

	vs := &fakeVectorStore{}
	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMetrics(metrics),
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
		t.Fatal("expected 2 ready")
	}

	// Exactly one Embed call — second ref deduped against the in-flight hash.
	if got := emb.calls.Load(); got != 1 {
		t.Errorf("Embed calls: want 1 (dedup), got %d", got)
	}

	// node_embeddings has exactly 1 row.
	var nec int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&nec)
	if nec != 1 {
		t.Errorf("node_embeddings rows: want 1 (dedup by content_hash), got %d", nec)
	}

	// Both refs share the same content_hash.
	var hashA, hashB string
	_ = db.QueryRow(`SELECT content_hash FROM node_embedding_refs WHERE node_id='a'`).Scan(&hashA)
	_ = db.QueryRow(`SELECT content_hash FROM node_embedding_refs WHERE node_id='b'`).Scan(&hashB)
	if hashA == "" || hashA != hashB {
		t.Errorf("content_hash mismatch: a=%q b=%q", hashA, hashB)
	}

	// Dedup counter incremented exactly once for the second ref.
	if v := testutil.ToFloat64(metrics.EmbedDedupHits); v != 1 {
		t.Errorf("EmbedDedupHits: want 1, got %v", v)
	}
}

// TestWorker_DedupCrossTick verifies that a ref enqueued AFTER the first
// embed has completed reuses the existing node_embeddings row without
// calling Embed (cross-tick dedup; same key, separate batches).
func TestWorker_DedupCrossTick(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "a", "r1", "main", "X", "function")

	emb := &fakeEmbedder{vector: []float32{0, 0, 0}, modelID: "m"}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMetrics(metrics),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Wait for first ref to be embedded.
	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 1
	}) {
		t.Fatal("first ref never embedded")
	}
	if got := emb.calls.Load(); got != 1 {
		t.Fatalf("after tick 1: Embed calls want 1, got %d", got)
	}

	// Now seed a sibling that projects to the same (kind, symbol_path).
	seedNode(t, db, "b", "r1", "main", "X", "function")

	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE node_id='b' AND state='ready'`).Scan(&n)
		return n == 1
	}) {
		t.Fatal("second ref never marked ready")
	}
	cancel()
	w.Wait()

	// Still exactly one Embed call — the second tick took the LookupExisting hit.
	if got := emb.calls.Load(); got != 1 {
		t.Errorf("after tick 2: Embed calls want 1 (cross-tick dedup), got %d", got)
	}
	if v := testutil.ToFloat64(metrics.EmbedDedupHits); v != 1 {
		t.Errorf("EmbedDedupHits: want 1, got %v", v)
	}
	var nec int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&nec)
	if nec != 1 {
		t.Errorf("node_embeddings rows: want 1, got %d", nec)
	}
}

// TestWorker_DistinctKeysCallEmbedIndependently verifies that distinct
// (kind, symbol_path) projections each result in their own Embed call (no
// false-positive dedup).
func TestWorker_DistinctKeysCallEmbedIndependently(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "a", "r1", "main", "pkg.A", "function")
	seedNode(t, db, "b", "r1", "main", "pkg.B", "function")
	seedNode(t, db, "c", "r1", "main", "pkg.A", "method") // same symbol, different kind

	emb := &fakeEmbedder{vector: []float32{1, 2, 3}, modelID: "m"}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMetrics(metrics),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 3
	}) {
		t.Fatalf("not all refs ready; embed=%d", emb.calls.Load())
	}
	cancel()
	w.Wait()

	if got := emb.calls.Load(); got != 3 {
		t.Errorf("Embed calls: want 3 (distinct keys), got %d", got)
	}
	if v := testutil.ToFloat64(metrics.EmbedDedupHits); v != 0 {
		t.Errorf("EmbedDedupHits: want 0 (no collisions), got %v", v)
	}
	var nec int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&nec)
	if nec != 3 {
		t.Errorf("node_embeddings rows: want 3, got %d", nec)
	}
}

// TestWorker_ModelIDChangeForcesFreshEmbed verifies that the model ID is
// part of the content_hash: the same embed_text under a different model
// produces a fresh Embed call rather than reusing the prior row.
func TestWorker_ModelIDChangeForcesFreshEmbed(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "a", "r1", "main", "X", "function")

	// First worker: model="old"
	embOld := &fakeEmbedder{vector: []float32{0, 0, 0}, modelID: "old"}
	w1 := mustNewWorker(t, repo, embOld, &fakeVectorStore{},
		embedder.WithInterval(5*time.Millisecond))
	ctx1, cancel1 := context.WithCancel(context.Background())
	w1.Start(ctx1)
	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 1
	}) {
		t.Fatal("tick 1 never completed")
	}
	cancel1()
	w1.Wait()

	// Seed a sibling with identical (kind, symbol_path).
	seedNode(t, db, "b", "r1", "main", "X", "function")

	// Second worker: model="new" — same embed_text, different modelID.
	embNew := &fakeEmbedder{vector: []float32{1, 1, 1}, modelID: "new"}
	w2 := mustNewWorker(t, repo, embNew, &fakeVectorStore{},
		embedder.WithInterval(5*time.Millisecond))
	ctx2, cancel2 := context.WithCancel(context.Background())
	w2.Start(ctx2)
	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE node_id='b' AND state='ready'`).Scan(&n)
		return n == 1
	}) {
		t.Fatal("tick 2 never completed")
	}
	cancel2()
	w2.Wait()

	// New model must NOT reuse the old row.
	if got := embNew.calls.Load(); got != 1 {
		t.Errorf("model change: Embed calls want 1 (fresh), got %d", got)
	}
	var nec int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&nec)
	if nec != 2 {
		t.Errorf("node_embeddings rows: want 2 (per-model), got %d", nec)
	}
}

func TestWorker_CtxCancelStopsCleanly(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	emb := &fakeEmbedder{vector: []float32{1}, modelID: "m"}
	vs := &fakeVectorStore{}
	w := mustNewWorker(t, repo, emb, vs, embedder.WithInterval(5*time.Millisecond))

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

	w := mustNewWorker(t, repo,
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

	w := mustNewWorker(t, repo, emb, vs,
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

// TestWorker_RateLimitThrottlesEmbedCalls verifies that WithRatePerSec(r)
// installs a token-bucket limiter that gates each Embed call. With r=5 and
// >=N pending rows, the wall time to issue N Embed calls must be at least
// roughly (N-1)/r seconds. We use a coarse lower bound to avoid flakiness:
// a bucket of size 1 means the first call goes through immediately and the
// remaining (N-1) calls each wait ~1/r seconds.
func TestWorker_RateLimitThrottlesEmbedCalls(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	const n = 6
	for i := range n {
		seedNode(t, db, "n"+string(rune('a'+i)), "r1", "main", "S"+string(rune('a'+i)), "function")
	}

	emb := &fakeEmbedder{vector: []float32{1, 2, 3}, modelID: "m"}
	vs := &fakeVectorStore{}

	const rps = 5.0
	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithRatePerSec(rps),
		embedder.WithBatchSize(n),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	w.Start(ctx)

	ok := waitForCondition(t, 5*time.Second, func() bool {
		return emb.calls.Load() >= int64(n)
	})
	elapsed := time.Since(start)
	cancel()
	w.Wait()

	if !ok {
		t.Fatalf("only %d Embed calls observed; want >=%d", emb.calls.Load(), n)
	}

	// Lower bound: bucket size is 1, so (n-1) tokens are awaited at 1/r each.
	// Allow a generous fudge — assert >= 60% of theoretical minimum.
	minWant := time.Duration(float64(n-1) / rps * float64(time.Second) * 0.6)
	if elapsed < minWant {
		t.Fatalf("rate limiter did not throttle: elapsed=%s want>=%s (n=%d, rps=%v)", elapsed, minWant, n, rps)
	}
}

// TestWorker_RateLimitCtxCancelUnwinds verifies that cancelling the ctx
// while a goroutine is blocked inside limiter.Wait returns cleanly and the
// worker shuts down promptly.
func TestWorker_RateLimitCtxCancelUnwinds(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	// Seed many nodes so the limiter must wait between calls.
	for i := range 20 {
		seedNode(t, db, "n"+string(rune('a'+i)), "r1", "main", "S"+string(rune('a'+i)), "function")
	}

	emb := &fakeEmbedder{vector: []float32{1}, modelID: "m"}
	vs := &fakeVectorStore{}

	// Very slow rate — guarantees the worker is parked in limiter.Wait.
	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithRatePerSec(0.5), // 1 call per 2 seconds
		embedder.WithBatchSize(20),
	)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// Let the worker drain one ref and then sit in Wait for the next token.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()

	exited := make(chan struct{})
	go func() { w.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("worker did not exit promptly after ctx cancel; elapsed=%s", time.Since(start))
	}
}

// TestWorker_RateLimitZeroMeansUnlimited verifies that WithRatePerSec(0)
// disables the limiter entirely (no gating). We assert by issuing many
// calls and observing they complete much faster than even a generous
// default rate would allow.
func TestWorker_RateLimitZeroMeansUnlimited(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	const n = 20
	for i := range n {
		seedNode(t, db, "n"+string(rune('a'+i)), "r1", "main", "S"+string(rune('a'+i)), "function")
	}

	emb := &fakeEmbedder{vector: []float32{1}, modelID: "m"}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithRatePerSec(0), // unlimited
		embedder.WithBatchSize(n),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	w.Start(ctx)

	if !waitForCondition(t, 2*time.Second, func() bool {
		return emb.calls.Load() >= int64(n)
	}) {
		t.Fatalf("only %d Embed calls observed; want >=%d", emb.calls.Load(), n)
	}
	elapsed := time.Since(start)
	cancel()
	w.Wait()

	// At default 10/s, 20 calls would take ~1.9s. Unlimited should be well
	// under 500ms even with SQLite and scheduling jitter.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("WithRatePerSec(0) should be unlimited; elapsed=%s", elapsed)
	}
}

// alwaysErrEmbedder always returns an error for the given target text;
// other texts succeed. Used to drive the retry counter without affecting
// siblings.
type alwaysErrEmbedder struct {
	calls   atomic.Int64
	failOn  string
	vector  []float32
	modelID string
	err     error
}

func (a *alwaysErrEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	a.calls.Add(1)
	if text == a.failOn {
		return nil, a.err
	}
	out := make([]float32, len(a.vector))
	copy(out, a.vector)
	return out, nil
}

func (a *alwaysErrEmbedder) ModelID() string { return a.modelID }

// TestWorker_RetryBumpsAttempts verifies that a single Embed error bumps
// attempts but leaves state='pending' so the row is drained again.
func TestWorker_RetryBumpsAttempts(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "n1", "r1", "main", "pkg.F", "function")

	emb := &alwaysErrEmbedder{
		failOn:  "function pkg.F f.go go",
		vector:  []float32{1, 2, 3},
		modelID: "m",
		err:     errors.New("boom"),
	}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMaxAttempts(3),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	// Wait for at least one attempt to be recorded.
	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT attempts FROM node_embedding_refs WHERE node_id='n1'`).Scan(&n)
		return n >= 1
	}) {
		t.Fatalf("attempts never bumped")
	}

	// Row must remain pending after a single failure (budget=3).
	var state string
	_ = db.QueryRow(`SELECT state FROM node_embedding_refs WHERE node_id='n1'`).Scan(&state)
	if state != "pending" {
		t.Errorf("after 1 failure: state want pending, got %q", state)
	}

	cancel()
	w.Wait()
}

// TestWorker_RetryExhaustionFlipsToFailed verifies that after maxAttempts
// errors the row's state becomes 'failed' and FetchPending excludes it.
func TestWorker_RetryExhaustionFlipsToFailed(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "doomed", "r1", "main", "pkg.D", "function")

	emb := &alwaysErrEmbedder{
		failOn:  "function pkg.D f.go go",
		vector:  []float32{1},
		modelID: "m",
		err:     errors.New("permanent"),
	}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMaxAttempts(3),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	if !waitForCondition(t, 2*time.Second, func() bool {
		var state string
		_ = db.QueryRow(`SELECT state FROM node_embedding_refs WHERE node_id='doomed'`).Scan(&state)
		return state == "failed"
	}) {
		var state string
		var attempts int
		_ = db.QueryRow(`SELECT state, attempts FROM node_embedding_refs WHERE node_id='doomed'`).
			Scan(&state, &attempts)
		t.Fatalf("row never flipped to failed; state=%q attempts=%d calls=%d",
			state, attempts, emb.calls.Load())
	}

	var attempts int
	_ = db.QueryRow(`SELECT attempts FROM node_embedding_refs WHERE node_id='doomed'`).Scan(&attempts)
	if attempts < 3 {
		t.Errorf("attempts: want >=3, got %d", attempts)
	}

	// FetchPending must not return failed rows.
	pending, err := repo.FetchPending(ctx, 10)
	if err != nil {
		t.Fatalf("FetchPending: %v", err)
	}
	for _, p := range pending {
		if p.NodeID == "doomed" {
			t.Errorf("FetchPending returned a failed row: %+v", p)
		}
	}

	cancel()
	w.Wait()
}

// TestWorker_SiblingsUnaffectedByFailure verifies that a per-row failure
// in the same batch does not block sibling rows from succeeding (per-row
// isolation: regression check for 2.1).
func TestWorker_SiblingsUnaffectedByFailure(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "ok1", "r1", "main", "pkg.A", "function")
	seedNode(t, db, "bad", "r1", "main", "pkg.B", "function")
	seedNode(t, db, "ok2", "r1", "main", "pkg.C", "function")

	emb := &alwaysErrEmbedder{
		failOn:  "function pkg.B f.go go",
		vector:  []float32{0.1, 0.2, 0.3},
		modelID: "m",
		err:     errors.New("transient"),
	}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs,
		embedder.WithInterval(5*time.Millisecond),
		embedder.WithMaxAttempts(3),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	if !waitForCondition(t, 2*time.Second, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='ready'`).Scan(&n)
		return n == 2
	}) {
		t.Fatalf("siblings never reached ready; calls=%d", emb.calls.Load())
	}

	// 'bad' eventually exhausts its budget and flips to failed.
	if !waitForCondition(t, 2*time.Second, func() bool {
		var s string
		_ = db.QueryRow(`SELECT state FROM node_embedding_refs WHERE node_id='bad'`).Scan(&s)
		return s == "failed"
	}) {
		t.Fatalf("bad row never flipped to failed")
	}

	cancel()
	w.Wait()
}

// TestWorker_NormalizesVectorsBeforeStorage asserts the worker L2-normalizes
// every embedding before it reaches VectorStorage and node_embeddings.
// Embedding models such as nomic-embed-text return vectors with norm far
// from 1.0; auto-link's score = 1/(1+L2dist) only yields meaningful
// thresholds for unit vectors. See solov2-uug.
func TestWorker_NormalizesVectorsBeforeStorage(t *testing.T) {
	db := openSchemaDB(t)
	repo := infsqlite.NewEmbeddingRefsRepo(db, db)

	seedNode(t, db, "n1", "r1", "main", "pkg.A", "function")

	// fakeEmbedder yields a clearly non-unit vector (it perturbs index 0
	// to the text's last rune, so norm is well above 1).
	emb := &fakeEmbedder{vector: []float32{3, 4}, modelID: "test-model"}
	vs := &fakeVectorStore{}

	w := mustNewWorker(t, repo, emb, vs, embedder.WithInterval(5*time.Millisecond))
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
		t.Fatalf("ref never reached ready")
	}

	var sawRow bool
	for _, c := range vs.snapshot() {
		for _, r := range c.rows {
			sawRow = true
			norm := vectorNorm(r.Vector)
			if math.Abs(norm-1.0) > 1e-5 {
				t.Errorf("upserted vector for %s not unit-norm: norm=%v vec=%v", r.NodeID, norm, r.Vector)
			}
		}
	}
	if !sawRow {
		t.Fatal("vector store saw no rows")
	}
}

func vectorNorm(v []float32) float64 {
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	return math.Sqrt(sq)
}

// Compile-time check our embedder satisfies the port.
var _ ports.EmbeddingProvider = (*fakeEmbedder)(nil)
var _ ports.EmbeddingProvider = (*blockingEmbedder)(nil)
var _ ports.EmbeddingProvider = (*alwaysErrEmbedder)(nil)
var _ ports.VectorStorage = (*fakeVectorStore)(nil)
