package autolink_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/observability"

	_ "modernc.org/sqlite"
)

// ---- fakes ----

type fakeRefs struct {
	hashes       map[string]string // nodeID -> hash
	notReady     map[string]bool   // nodeID -> true (forces ready=false)
	hashErr      error
	embeds       map[string][]float32 // hash -> vector
	embedNotF    map[string]bool      // hash -> missing
	embedErr     error
	contentCalls int
	lookupCalls  int
}

func (f *fakeRefs) ContentHashForNode(_ context.Context, _, _, nodeID string) (string, bool, error) {
	f.contentCalls++
	if f.hashErr != nil {
		return "", false, f.hashErr
	}
	if f.notReady[nodeID] {
		return "", false, nil
	}
	h, ok := f.hashes[nodeID]
	if !ok {
		return "", false, nil
	}
	return h, true, nil
}

func (f *fakeRefs) LookupExisting(_ context.Context, hash string) ([]byte, int, bool, error) {
	f.lookupCalls++
	if f.embedErr != nil {
		return nil, 0, false, f.embedErr
	}
	if f.embedNotF[hash] {
		return nil, 0, false, nil
	}
	v, ok := f.embeds[hash]
	if !ok {
		return nil, 0, false, nil
	}
	return encodeF32LE(v), len(v), true, nil
}

func encodeF32LE(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

type fakeVectors struct {
	// Hits are returned in queue order — one Search call pops one element.
	// This keeps multi-source tests deterministic without needing to inspect
	// the input vector.
	queue [][]domain.Hit
	err   error
	calls int
	lastK int
}

func (fv *fakeVectors) UpsertEmbeddings(context.Context, string, string, []domain.EmbeddingRow) error {
	return nil
}
func (fv *fakeVectors) Reindex(context.Context, string, string) error { return nil }
func (fv *fakeVectors) LookupContentHashes(context.Context, string, string, []string) (map[string]string, error) {
	return nil, nil
}

func (fv *fakeVectors) Search(_ context.Context, _, _ string, _ []float32, k int, _ domain.Filter) ([]domain.Hit, error) {
	fv.calls++
	fv.lastK = k
	if fv.err != nil {
		return nil, fv.err
	}
	if len(fv.queue) == 0 {
		return nil, nil
	}
	hh := fv.queue[0]
	fv.queue = fv.queue[1:]
	return hh, nil
}

// ---- tests ----

// mustNewLinker constructs a Linker and fails the test if the constructor
// returns an error. Used by the happy-path tests that pass non-nil deps.
func mustNewLinker(t *testing.T, refs autolink.EmbeddingLookup, vectors ports.VectorStorage, opts ...autolink.Option) *autolink.Linker {
	t.Helper()
	l, err := autolink.NewLinker(refs, vectors, opts...)
	if err != nil {
		t.Fatalf("autolink.NewLinker: %v", err)
	}
	return l
}

func TestNewLinker_NilDependencyReturnsTypedError(t *testing.T) {
	refs := &fakeRefs{}
	vs := &fakeVectors{}

	cases := []struct {
		name    string
		refs    autolink.EmbeddingLookup
		vectors ports.VectorStorage
	}{
		{"nil refs", nil, vs},
		{"nil vectors", refs, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := autolink.NewLinker(tc.refs, tc.vectors)
			if l != nil {
				t.Errorf("expected nil *Linker, got %v", l)
			}
			if !errors.Is(err, autolink.ErrMissingDependency) {
				t.Fatalf("err = %v, want wraps ErrMissingDependency", err)
			}
		})
	}
}

func TestCandidates_HappyPath_ThresholdAndSelfFilter(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes: map[string]string{"A": "ha"},
		embeds: map[string][]float32{"ha": {1, 0, 0}},
	}
	vs := &fakeVectors{
		queue: [][]domain.Hit{{
			{NodeID: "A", Score: 1.0}, // self
			{NodeID: "B", Score: 0.95},
			{NodeID: "C", Score: 0.55}, // below default 0.60
			{NodeID: "D", Score: 0.50}, // below default 0.60
		}},
	}
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	l := mustNewLinker(t, refs, vs, autolink.WithMetrics(m))

	got, err := l.Candidates(context.Background(), "r1", "main", []string{"A"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.SourceNodeID != "A" || c.TargetNodeID != "B" || c.Score != 0.95 || c.RepoID != "r1" || c.Branch != "main" {
		t.Errorf("unexpected candidate: %+v", c)
	}
	if vs.lastK != autolink.DefaultTopK+1 {
		t.Errorf("Search k: want %d, got %d", autolink.DefaultTopK+1, vs.lastK)
	}
	if v := testutil.ToFloat64(m.AutolinkCandidates.WithLabelValues("r1")); v != 1 {
		t.Errorf("counter: want 1, got %v", v)
	}
}

func TestCandidates_TopKCap(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes: map[string]string{"A": "ha"},
		embeds: map[string][]float32{"ha": {1, 0}},
	}
	vs := &fakeVectors{
		queue: [][]domain.Hit{{
			{NodeID: "A", Score: 1.0},
			{NodeID: "B", Score: 0.99},
			{NodeID: "C", Score: 0.98},
			{NodeID: "D", Score: 0.97},
		}},
	}
	l := mustNewLinker(t, refs, vs, autolink.WithTopK(2), autolink.WithThreshold(0.5))

	got, err := l.Candidates(context.Background(), "r1", "main", []string{"A"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(got))
	}
	if got[0].TargetNodeID != "B" || got[1].TargetNodeID != "C" {
		t.Errorf("ordering wrong: %+v", got)
	}
}

func TestCandidates_SkipNotReady(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes:   map[string]string{"A": "ha"},
		notReady: map[string]bool{"A": true},
	}
	vs := &fakeVectors{}
	l := mustNewLinker(t, refs, vs)

	got, err := l.Candidates(context.Background(), "r1", "main", []string{"A"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0, got %d", len(got))
	}
	if vs.calls != 0 {
		t.Errorf("Search should not be called for unready node, calls=%d", vs.calls)
	}
}

func TestCandidates_SkipMissingEmbedding(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes:    map[string]string{"A": "ha"},
		embedNotF: map[string]bool{"ha": true},
	}
	vs := &fakeVectors{}
	l := mustNewLinker(t, refs, vs)

	got, err := l.Candidates(context.Background(), "r1", "main", []string{"A"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0, got %d", len(got))
	}
	if vs.calls != 0 {
		t.Errorf("Search should not run when embedding missing")
	}
}

func TestCandidates_EmptyInput(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{}
	vs := &fakeVectors{}
	l := mustNewLinker(t, refs, vs)

	got, err := l.Candidates(context.Background(), "r1", "main", nil)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestCandidates_MultipleSources_Union(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes: map[string]string{"A": "ha", "B": "hb"},
		embeds: map[string][]float32{"ha": {1, 0}, "hb": {0, 1}},
	}
	vs := &fakeVectors{
		queue: [][]domain.Hit{
			{{NodeID: "A", Score: 1.0}, {NodeID: "X", Score: 0.91}},
			{{NodeID: "B", Score: 1.0}, {NodeID: "Y", Score: 0.92}},
		},
	}
	l := mustNewLinker(t, refs, vs, autolink.WithTopK(1))

	got, err := l.Candidates(context.Background(), "r1", "main", []string{"A", "B"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}
	if got[0].SourceNodeID != "A" || got[0].TargetNodeID != "X" {
		t.Errorf("first candidate wrong: %+v", got[0])
	}
	if got[1].SourceNodeID != "B" || got[1].TargetNodeID != "Y" {
		t.Errorf("second candidate wrong: %+v", got[1])
	}
}

func TestCandidates_SearchErrorPropagates(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes: map[string]string{"A": "ha"},
		embeds: map[string][]float32{"ha": {1, 0}},
	}
	want := errors.New("boom")
	vs := &fakeVectors{err: want}
	l := mustNewLinker(t, refs, vs)

	_, err := l.Candidates(context.Background(), "r1", "main", []string{"A"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("error chain wrong: %v", err)
	}
}

func TestCandidates_CounterIncrementsByEmitted(t *testing.T) {
	t.Parallel()
	refs := &fakeRefs{
		hashes: map[string]string{"A": "ha"},
		embeds: map[string][]float32{"ha": {1, 0}},
	}
	vs := &fakeVectors{
		queue: [][]domain.Hit{{
			{NodeID: "A", Score: 1.0},
			{NodeID: "B", Score: 0.95},
			{NodeID: "C", Score: 0.94},
			{NodeID: "D", Score: 0.93},
		}},
	}
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	l := mustNewLinker(t, refs, vs, autolink.WithMetrics(m))

	_, err := l.Candidates(context.Background(), "r9", "main", []string{"A"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if v := testutil.ToFloat64(m.AutolinkCandidates.WithLabelValues("r9")); v != 3 {
		t.Errorf("counter: want 3, got %v", v)
	}
}

// ---- integration test against the real EmbeddingRefsRepo ----

func TestCandidates_Integration_RealRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := sqlite.NewEmbeddingRefsRepo(db, db)

	// Seed: repo "r1" branch "main" with two nodes A (ready) and B (pending).
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("repo: %v", err)
	}
	insNode := func(id string) {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			content_hash, last_promoted_at, actor_id, actor_kind
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			id, "main", "r1", "go", "function", id, "f.go",
			"h", now, "test", "system"); err != nil {
			t.Fatalf("node %s: %v", id, err)
		}
	}
	insNode("A")
	insNode("B")
	// A: insert embedding bytes and a ready ref.
	vecA := []float32{1, 0, 0}
	if _, err := db.Exec(
		`INSERT INTO node_embeddings(content_hash, model, dim, embedding, created_at) VALUES (?,?,?,?,?)`,
		"ha", "m", len(vecA), encodeF32LE(vecA), now); err != nil {
		t.Fatalf("emb A: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at, embedded_at)
		 VALUES (?,?,?,?,?)`,
		"A", "ha", "ready", now, now); err != nil {
		t.Fatalf("ref A: %v", err)
	}
	// B: pending ref, no hash.
	if _, err := db.Exec(
		`INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`,
		"B", now); err != nil {
		t.Fatalf("ref B: %v", err)
	}

	// Fake vector store returning [self, B(0.95), C(0.55) below default].
	vs := &fakeVectors{
		queue: [][]domain.Hit{{
			{NodeID: "A", Score: 1.0},
			{NodeID: "B", Score: 0.95},
			{NodeID: "C", Score: 0.55},
		}},
	}
	l := mustNewLinker(t, repo, vs)

	got, err := l.Candidates(context.Background(), "r1", "main", []string{"A", "B"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate (A→B), got %d: %+v", len(got), got)
	}
	if got[0].SourceNodeID != "A" || got[0].TargetNodeID != "B" || got[0].Score != 0.95 {
		t.Errorf("unexpected: %+v", got[0])
	}
	// B was pending → must have been skipped before any Search call for B.
	if vs.calls != 1 {
		t.Errorf("Search calls: want 1 (only A), got %d", vs.calls)
	}
}

// TestEmbeddingRefsRepo_ContentHashForNode is sibling coverage that proves
// the new port method's branch/scope filtering at the SQL level. Lives in
// the autolink test file (not embedding_refs_repo_test.go) to keep adjacent
// fixtures (encodeF32LE etc.) in one place; the repo's own test file owns
// the other methods.
func TestEmbeddingRefsRepo_ContentHashForNode_NoCrossRepoLeak(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := sqlite.NewEmbeddingRefsRepo(db, db)

	now := time.Now().UnixMilli()
	mustExec(t, db, `INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`, "r1", "/x", now)
	mustExec(t, db, `INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"A", "main", "r1", "go", "function", "A", "f.go", "h", now, "t", "system")
	mustExec(t, db, `INSERT INTO node_embeddings(content_hash, model, dim, embedding, created_at) VALUES (?,?,?,?,?)`,
		"ha", "m", 1, []byte{0, 0, 0, 0}, now)
	mustExec(t, db, `INSERT INTO node_embedding_refs (node_id, content_hash, state, enqueued_at, embedded_at) VALUES (?,?,?,?,?)`,
		"A", "ha", "ready", now, now)

	// Hit: matches scope.
	hash, ready, err := repo.ContentHashForNode(context.Background(), "r1", "main", "A")
	if err != nil {
		t.Fatalf("ContentHashForNode: %v", err)
	}
	if !ready || hash != "ha" {
		t.Errorf("hit: want (ha,true), got (%q,%v)", hash, ready)
	}

	// Miss: wrong branch.
	_, ready, err = repo.ContentHashForNode(context.Background(), "r1", "feature", "A")
	if err != nil || ready {
		t.Errorf("wrong-branch must miss with no err, got (ready=%v, err=%v)", ready, err)
	}

	// Miss: wrong repo.
	_, ready, err = repo.ContentHashForNode(context.Background(), "r2", "main", "A")
	if err != nil || ready {
		t.Errorf("wrong-repo must miss with no err, got (ready=%v, err=%v)", ready, err)
	}

	// Miss: unknown node.
	_, ready, err = repo.ContentHashForNode(context.Background(), "r1", "main", "Z")
	if err != nil || ready {
		t.Errorf("unknown node must miss with no err, got (ready=%v, err=%v)", ready, err)
	}
}

func TestEmbeddingRefsRepo_ContentHashForNode_PendingNotReady(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := sqlite.NewEmbeddingRefsRepo(db, db)

	now := time.Now().UnixMilli()
	mustExec(t, db, `INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`, "r1", "/x", now)
	mustExec(t, db, `INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"P", "main", "r1", "go", "function", "P", "f.go", "h", now, "t", "system")
	mustExec(t, db, `INSERT INTO node_embedding_refs (node_id, state, enqueued_at) VALUES (?, 'pending', ?)`, "P", now)

	hash, ready, err := repo.ContentHashForNode(context.Background(), "r1", "main", "P")
	if err != nil {
		t.Fatalf("ContentHashForNode: %v", err)
	}
	if ready || hash != "" {
		t.Errorf("pending must be ready=false hash=\"\", got (%q,%v)", hash, ready)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
