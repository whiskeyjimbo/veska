//go:build eval

// Package recall's eval test: drives a real search.Service against an
// in-memory SQLite NodeLookup adapter and an in-process VectorStorage
// (the sqlite-vec linear-scan backend by default, per ADR-S0015), with
// a deterministic synthetic corpus. Build-tag-gated so plain CI runs
// (`go test ./...`) skip this end-to-end driver — it stays available
// via `go test -tags=eval` from the eval-recall make target.
package recall

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// TestRecall is the end-to-end eval harness. It builds a synthetic
// corpus, generates vectors via the FakeEmbedder (quick mode) or
// replays a previously-cached real-Ollama fixture, inserts everything
// into a real VectorStorage + in-memory SQLite (via the existing
// NodeLookupRepo), drives 100 cluster-center queries through
// search.Service.Semantic, and emits a JSON summary.
//
// Modes (env):
//   - RECALL_POP=N             — total population (default 1000)
//   - RECALL_GENERATE=1        — allow real-Ollama fixture seeding
//     (NB: ollama path is not implemented in this milestone; setting
//     RECALL_GENERATE=1 without a fake-compatible fixture is a no-op
//     for the quick-mode path)
//
// The quick-mode (<= 5000) path uses the FakeEmbedder directly without
// requiring a fixture or Ollama. Larger populations require a fixture
// file at fixtures/embeddings_<pop>.bin; if absent and RECALL_GENERATE
// is not set, the test SKIPS (it does not fail).
func TestRecall(t *testing.T) {
	pop := envInt("RECALL_POP", 1000)
	generate := os.Getenv("RECALL_GENERATE") == "1"

	const (
		clusters    = 100
		k           = 10
		fixtureRoot = "fixtures"
	)
	nodesPerCluster := pop / clusters
	if nodesPerCluster < 1 {
		t.Fatalf("RECALL_POP=%d too small: need at least %d (clusters)", pop, clusters)
	}
	pop = clusters * nodesPerCluster // round to exact multiple

	corpus := GenerateCorpus(clusters, nodesPerCluster)

	// --- choose embedder + obtain vectors ----------------------------------
	embedderName := "fake"
	quickMode := pop <= 5000
	var nodeVecs []float32
	dim := FakeEmbeddingDim

	fixturePath := FixturePath(fixtureRoot, pop)
	if _, err := os.Stat(fixturePath); err == nil {
		// Replay cached fixture (could be either fake-seeded or
		// ollama-seeded — we don't distinguish on disk; the embedder
		// label below reflects the runtime path actually taken).
		d, vecs, rerr := ReadFixture(fixturePath)
		if rerr != nil {
			t.Fatalf("ReadFixture(%s): %v", fixturePath, rerr)
		}
		if d != dim {
			// A fixture from a different embedder is fine for replay
			// — we just need the dim to be consistent across nodes &
			// queries below. Re-tag as "fixture".
			embedderName = "fixture"
			dim = d
		}
		nodeVecs = vecs
	} else if quickMode {
		// Fake embedder: deterministic, no I/O, fast.
		nodeVecs = make([]float32, 0, pop*dim)
		for _, n := range corpus.Nodes {
			nodeVecs = append(nodeVecs, synthcorpus.FakeEmbed(n.Text)...)
		}
		if generate {
			// Persist for reproducibility — harmless when seeded from
			// fake; gives the M3-close report a deterministic artefact.
			if err := WriteFixture(fixturePath, dim, nodeVecs); err != nil {
				t.Logf("WriteFixture(%s): %v (continuing in-memory)", fixturePath, err)
			}
		}
	} else {
		// Large population, no fixture, no opt-in: this is the
		// "deferred to milestone close" path. SKIP, don't fail.
		t.Skipf("recall: fixture %s not present and pop=%d > quick-mode cap; set RECALL_GENERATE=1 + provide ollama-seeded fixture to run",
			fixturePath, pop)
		return
	}

	if got := len(nodeVecs) / dim; got != pop {
		t.Fatalf("vector count mismatch: have %d vectors, expected %d", got, pop)
	}

	// --- wire SQLite + VectorStorage --------------------------------------
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	backupDir := filepath.Join(tmpDir, "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("sqlite.OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		repoID = "recall-eval"
		branch = "main"
	)
	seedNodes(t, db, repoID, branch, corpus.Nodes)

	vstore, err := vector.NewVectorStorage(vector.BackendSQLiteVec, "")
	if err != nil {
		t.Fatalf("vector.NewVectorStorage: %v", err)
	}
	backendName := string(vector.BackendSQLiteVec)

	rows := make([]domain.EmbeddingRow, pop)
	for i, n := range corpus.Nodes {
		rows[i] = domain.EmbeddingRow{
			NodeID:      n.NodeID,
			ContentHash: "h-" + n.NodeID,
			ModelID:     "fake-hash-v1",
			Vector:      append([]float32(nil), nodeVecs[i*dim:(i+1)*dim]...),
		}
	}
	if err := vstore.UpsertEmbeddings(context.Background(), repoID, branch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	// --- run queries through real search.Service ---------------------------
	embedder := FakeEmbedder{}
	nodeLookup := sqlite.NewNodeLookupRepo(db)
	svc := search.NewService(embedder, vstore, nodeLookup)

	truth := corpus.TruthByCluster()
	perQuery := make([]float64, 0, clusters)
	latencies := make([]time.Duration, 0, clusters)

	ctx := context.Background()
	for cluster, q := range corpus.CenterQueries {
		start := time.Now()
		resp, err := svc.Semantic(ctx, repoID, branch, q, k, domain.Filter{})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Semantic(cluster %d): %v", cluster, err)
		}
		ids := make([]string, len(resp.Results))
		for i, r := range resp.Results {
			ids[i] = r.NodeID
		}
		perQuery = append(perQuery, RecallAtK(ids, truth[cluster], k))
		latencies = append(latencies, elapsed)
	}

	mean := MeanRecall(perQuery)
	p95 := P95Latency(latencies)

	if mean <= 0 {
		t.Fatalf("mean_recall is zero — fake embedder did not produce cluster-aligned vectors (pop=%d)", pop)
	}

	// --- emit JSON + single-line summary -----------------------------------
	res := Result{
		Population:      pop,
		Clusters:        clusters,
		NodesPerCluster: nodesPerCluster,
		Queries:         len(corpus.CenterQueries),
		MeanRecall:      mean,
		P95LatencyMs:    float64(p95.Microseconds()) / 1000.0,
		Embedder:        embedderName,
		Backend:         backendName,
		Timestamp:       time.Now().UTC(),
	}
	outPath := filepath.Join(t.TempDir(), "recall_results.json")
	if err := WriteJSON(outPath, res); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	// Also drop a copy at the well-known location relative to the
	// package so `make eval-recall` can pick it up.
	_ = WriteJSON("recall_results.json", res)

	fmt.Printf("RECALL pop=%d mean_recall=%.2f p95_latency_ms=%.2f embedder=%s backend=%s\n",
		pop, mean, res.P95LatencyMs, embedderName, backendName)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// seedNodes inserts a synthetic-corpus batch into the nodes table so
// NodeLookupRepo can hydrate the IDs that VectorStorage.Search returns.
// The schema columns mirror the production layout used by ingestion.
func seedNodes(t *testing.T, db *sql.DB, repoID, branch string, nodes []SyntheticNode) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()

	for _, n := range nodes {
		if _, err := stmt.Exec(
			n.NodeID, branch, repoID, "go", n.Kind, n.SymbolPath, n.FilePath,
			1, 1, "h-"+n.NodeID, now, "recall-eval", "system",
		); err != nil {
			t.Fatalf("insert node %s: %v", n.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
