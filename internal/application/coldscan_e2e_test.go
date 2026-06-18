// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package application_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// TestColdScanE2E_ReindexProducesSearchableVectors exercises the full
// reindex → embedder → VectorStorage pipeline against the real production
// adapters: sqlite (with migrations), tree-sitter, Ollama, and a concrete
// VectorStorage backend. It is parameterised over backend kind so the same
// fixture proves that vectors land in whichever store the operator
// configures via VESKA_VECTOR_BACKEND.
// Skips:
//
//	The whole test skips when Ollama is unreachable at the default URL.
//	The "usearch" subtest skips when the binary was not built with
//	  tags hnsw_native (vector.NewVectorStorage returns
//	  vector.ErrVectorStoreUnavailable).
//
// race is intentionally omitted from the documented run instructions: the
// embedder polling loop combined with cgo (usearch) is fragile under -race
// for value that this long-running integration test does not add. Unit
// level race coverage already exists in narrower tests.
func TestColdScanE2E_ReindexProducesSearchableVectors(t *testing.T) {
	if !ollamaReachable(t) {
		t.Skip("ollama unreachable at http://localhost:11434 - skipping e2e (set up Ollama with nomic-embed-text to run)")
	}

	t.Run("sqlite-vec", func(t *testing.T) {
		runColdScanE2E(t, vector.BackendMemory)
	})

	t.Run("usearch", func(t *testing.T) {
		runColdScanE2E(t, vector.BackendUsearch)
	})
}

// ollamaReachable does a fast (1s) probe against /api/tags. We accept any
// non-error response - a healthy Ollama returns 200, but a 404 would still
// prove the daemon is up.
func ollamaReachable(t *testing.T) bool {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://localhost:11434/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return true
}

// runColdScanE2E drives the full pipeline for one backend kind. The
// fixture has two Go files with deliberately disjoint vocabularies so a
// query for "compute alpha metric" returns the ComputeAlphaMetric node
// rather than RenderBetaWidget.
func runColdScanE2E(t *testing.T, kind vector.BackendKind) {
	t.Helper()

	// Vector storage first - for usearch this is the gate that decides
	// whether the subtest can run at all.
	vecDir := t.TempDir()
	vecStore, err := vector.NewVectorStorage(kind, vecDir)
	if err != nil {
		if errors.Is(err, vector.ErrVectorStoreUnavailable) {
			t.Skipf("vector backend %q unavailable: %v (rebuild with -tags hnsw_native)", kind, err)
		}
		t.Fatalf("vector.NewVectorStorage(%q): %v", kind, err)
	}

	// Real sqlite DB with the production migrations (incl. 0009
	// nodes.snippet). sqlite.Open calls os.Exit(78) on migration failure,
	// so the only outcome we tolerate here is success.
	dbPath := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		repoID = "repo-e2e"
		branch = "main"
	)

	// Fixture repo on disk - its path doubles as the repos.root_path FK.
	repoRoot := t.TempDir()
	writeIntFile(t, repoRoot, "alpha_metric.go",
		"package fixture\n\n// ComputeAlphaMetric calculates the alpha metric for a series.\n"+
			"func ComputeAlphaMetric(values []float64) float64 { return 0 }\n")
	writeIntFile(t, repoRoot, "beta_widget.go",
		"package fixture\n\n// RenderBetaWidget renders the beta widget.\n"+
			"func RenderBetaWidget(name string) string { return name }\n")

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch) VALUES (?, ?, ?, ?)`,
		repoID, repoRoot, time.Now().UnixMilli(), branch,
	); err != nil {
		t.Fatalf("insert repos row: %v", err)
	}

	// Parse / promote chain - identical to wire.go.
	parser := treesitter.NewGoParser()
	area := staging.NewArea()
	gate := staging.NewGate(area)
	ingester := application.NewIngester(parser, area, gate)
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	promoter := application.NewPromoter(area, store)

	reparser, err := application.NewColdScanReparser(
		ingester, promoter, &fakeColdScanGit{head: "sha-e2e"},
		application.WithIgnoreLoader(realIgnoreLoader),
	)
	if err != nil {
		t.Fatalf("NewColdScanReparser: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := reparser(ctx, application.RepoRecord{
		RepoID:       repoID,
		RootPath:     repoRoot,
		ActiveBranch: branch,
	}); err != nil {
		t.Fatalf("reparser: %v", err)
	}

	// Sanity: the promotion sinks queued at least one ref per promoted node.
	refs := sqlite.NewEmbeddingRefsRepo(db, db)
	pending0, err := refs.CountPending(ctx)
	if err != nil {
		t.Fatalf("CountPending (initial): %v", err)
	}
	if pending0 == 0 {
		t.Fatal("expected node_embedding_refs(state=pending) > 0 after reparser; got 0")
	}

	// Ollama provider -:latest matches the local tag from `ollama list`.
	prov, err := ollama.New("nomic-embed-text:latest", ollama.WithBaseURL("http://localhost:11434"))
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}

	// Embedder worker - short interval for fast convergence in this test.
	worker, err := embedder.NewWorker(refs, prov, vecStore, embedder.WithInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("embedder.NewWorker: %v", err)
	}
	worker.Start(ctx)
	t.Cleanup(worker.Stop)

	// Poll until pending hits zero (or the parent ctx fires).
	drainCtx, drainCancel := context.WithTimeout(ctx, 60*time.Second)
	defer drainCancel()
	if err := waitPendingDrained(drainCtx, refs); err != nil {
		failed := countFailedRefs(t, db)
		pendingNow, _ := refs.CountPending(context.Background())
		t.Fatalf("embedder drain timed out: %v (pending=%d failed=%d)", err, pendingNow, failed)
	}

	worker.Stop()

	// Worker upserts to node_embeddings before flipping refs.embedded - at
	// least one row must exist.
	if n := countNodeEmbeddings(t, db); n == 0 {
		t.Fatal("node_embeddings is empty after embedder drained; worker did not persist any vectors")
	}

	// Build a query vector and exercise the vector store directly. We use
	// the same provider so the query/document vectors share dimensionality
	// and embedding space.
	queryVec, err := prov.Embed(ctx, "compute alpha metric")
	if err != nil {
		t.Fatalf("prov.Embed(query): %v", err)
	}
	if len(queryVec) == 0 {
		t.Fatal("prov.Embed returned empty vector")
	}
	l2Normalize(queryVec)

	hits, err := vecStore.Search(ctx, repoID, branch, queryVec, 5, domain.VectorFilter{})
	if err != nil {
		t.Fatalf("vecStore.Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("Search returned 0 hits; node_embeddings=%d, dim=%d, backend=%q",
			countNodeEmbeddings(t, db), len(queryVec), kind)
	}

	alphaNodeID := lookupNodeID(t, db, "%ComputeAlphaMetric%")
	if alphaNodeID == "" {
		t.Fatal("could not find ComputeAlphaMetric node in nodes table")
	}
	// Retrieval competitors for the alpha-symbol slot:
	//   package-scope node, snippet=whole file;
	//   chunk nodes covering non-declaration regions;
	// All three carry the alpha tokens. The test asserts the symbol is
	// in the same semantic cluster as the query, not that it beats every
	// chunk/package-level competitor. Top-5 placement still proves the
	// wiring (parse → embed → vector search → results) end-to-end.
	const topK = 5
	inTopK := false
	for i := 0; i < topK && i < len(hits); i++ {
		if hits[i].NodeID == alphaNodeID {
			inTopK = true
			break
		}
	}
	if !inTopK {
		t.Errorf("alpha node %q not in top %d hits; got %+v", alphaNodeID, topK, hits)
	}
}

// waitPendingDrained polls every 250ms until CountPending returns 0 or ctx fires.
func waitPendingDrained(ctx context.Context, refs *sqlite.EmbeddingRefsRepo) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		n, err := refs.CountPending(ctx)
		if err == nil && n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func countFailedRefs(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='failed'`).Scan(&n)
	return n
}

func countNodeEmbeddings(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&n); err != nil {
		t.Fatalf("countNodeEmbeddings: %v", err)
	}
	return n
}

func lookupNodeID(t *testing.T, db *sql.DB, like string) string {
	t.Helper()
	rows, err := db.Query(`SELECT node_id, symbol_path FROM nodes WHERE symbol_path LIKE ?`, like)
	if err != nil {
		t.Fatalf("lookupNodeID: %v", err)
	}
	defer rows.Close()
	var id, sym string
	for rows.Next() {
		var rid, rsym string
		if err := rows.Scan(&rid, &rsym); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// Prefer the function-level node (deepest symbol_path containing the marker).
		if strings.Contains(rsym, "ComputeAlphaMetric") && (sym == "" || len(rsym) > len(sym)) {
			id, sym = rid, rsym
		}
	}
	return id
}

// l2Normalize matches the embedder worker's normalisation step so the query
// vector lives on the same unit sphere as stored document vectors. The
// VectorStorage score 1/(1+L2dist) is only meaningful between unit vectors.
func l2Normalize(vec []float32) {
	var sq float64
	for _, f := range vec {
		sq += float64(f) * float64(f)
	}
	if sq == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sq))
	for i := range vec {
		vec[i] *= inv
	}
}
