// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Package autolink's eval test: drives a real autolink.Linker against
// an in-memory SQLite (with the real EmbeddingRefRepo adapter) and an
// in-process VectorStorage (the sqlite-vec linear-scan backend by
// default, per ), using a deterministic synthetic corpus.
// Build-tag-gated so plain CI runs (`go test./.`) skip this
// end-to-end driver - it stays available via `go test -tags=eval`
// from the eval-autolink-fp make target.
package autolink

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/recall"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// sharedFixtureDir is where the recall harness writes its Ollama-seeded
// fixture. Autolink replays from the same path so a single 50k
// generation seeds both gate-2 and gate-3.
const sharedFixtureDir = "../recall/fixtures"

// TestAutolinkFP is the end-to-end auto-link false-positive harness.
// It builds a synthetic corpus, generates deterministic vectors via
// FakeEmbedder (quick mode), wires them into a real VectorStorage and
// the real sqlite EmbeddingRefRepo, then runs autolink.Linker for each
// source node and computes the FP rate against the cluster ground
// truth.
// Modes (env):
//
//	AUTOLINK_POP=N - total population (default 1000).
//	AUTOLINK_THRESHOLD=X.XX - minimum similarity to admit a
//	  candidate (default 0.85, matching autolink.DefaultThreshold).
//	AUTOLINK_TOPK=K - per-source candidate cap (default 5,
//	  matching autolink.DefaultTopK).
//	RECALL_GENERATE=1 - persist a fixture for reproducibility
//	  (currently a no-op on this harness; reserved for future real
//	  Ollama seeding to mirror the recall harness).
func TestAutolinkFP(t *testing.T) {
	pop := envInt("AUTOLINK_POP", 1000)
	topK := envInt("AUTOLINK_TOPK", autolink.DefaultTopK)
	threshold := envFloat("AUTOLINK_THRESHOLD", float64(autolink.DefaultThreshold))

	const (
		repoID = "autolink-eval"
		branch = "main"
	)

	// VESKA_CORPUS=semantic switches to the per-cluster topic-vocabulary
	// corpus required for a meaningful FP measurement against real
	// embeddings; its cluster count is fixed by the hand-authored
	// vocabulary. The legacy corpus uses 100. The shared fixture path is
	// suffixed for semantic runs so the two corpora don't collide.
	semantic := os.Getenv("VESKA_CORPUS") == "semantic"
	clusters := 100
	if semantic {
		clusters = synthcorpus.SemanticClusterCount
	}
	nodesPerCluster := pop / clusters
	if nodesPerCluster < 1 {
		t.Fatalf("AUTOLINK_POP=%d too small: need at least %d (clusters)", pop, clusters)
	}
	pop = clusters * nodesPerCluster

	var corpus synthcorpus.Corpus
	if semantic {
		corpus = synthcorpus.GenerateSemanticCorpus(nodesPerCluster)
	} else {
		corpus = synthcorpus.GenerateCorpus(clusters, nodesPerCluster)
	}
	clusterOf := corpus.ClusterOf()

	// Resolve the embedding source: prefer the shared on-disk fixture
	// (so gate-2 + gate-3 share one real-Ollama generation), fall back
	// to the deterministic FakeEmbed when no fixture is present.
	vecOf, embedderName := loadEmbeddings(t, corpus.Nodes, pop, "fake", semantic)

	// wire SQLite + repos
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	backupDir := filepath.Join(tmpDir, "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("sqlite.OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	seedNodes(t, db, repoID, branch, corpus.Nodes)
	seedEmbeddings(t, db, corpus.Nodes, vecOf)

	// VectorStorage
	// VESKA_VECTOR_BACKEND selects the backend (default memory/memvec).
	// "usearch" requires the hnsw_native build tag and libusearch_c.so at
	// runtime. The memory backend is an O(N^2) linear scan
	// and exceeds budget at pop > ~10k for the autolink Candidates sweep.
	backendKind := vector.BackendKind(os.Getenv("VESKA_VECTOR_BACKEND"))
	if backendKind == "" {
		backendKind = vector.BackendMemory
	}
	vstore, err := vector.NewVectorStorage(backendKind, t.TempDir())
	if err != nil {
		t.Fatalf("vector.NewVectorStorage(%s): %v", backendKind, err)
	}
	backendName := string(backendKind)

	rows := make([]domain.EmbeddingRow, pop)
	modelID := "fake-hash-v1"
	if embedderName != "fake" {
		modelID = embedderName
	}
	for i, n := range corpus.Nodes {
		rows[i] = domain.EmbeddingRow{
			NodeID:      n.NodeID,
			ContentHash: contentHashFor(n.NodeID),
			ModelID:     modelID,
			Vector:      vecOf(i, n),
		}
	}
	if err := vstore.UpsertEmbeddings(context.Background(), repoID, branch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	// Linker
	refs := sqlite.NewEmbeddingRefsRepo(db, db)
	linker, err := autolink.NewLinker(refs, vstore,
		autolink.WithTopK(topK),
		autolink.WithThreshold(float32(threshold)),
	)
	if err != nil {
		t.Fatalf("autolink.NewLinker: %v", err)
	}

	// run Candidates for every source node
	ctx := context.Background()
	srcIDs := make([]string, len(corpus.Nodes))
	for i, n := range corpus.Nodes {
		srcIDs[i] = n.NodeID
	}
	cands, err := linker.Candidates(ctx, repoID, branch, srcIDs)
	if err != nil {
		t.Fatalf("linker.Candidates: %v", err)
	}

	// classify + compute FP rate
	pairs := make([]Pair, 0, len(cands))
	for _, c := range cands {
		srcK, ok1 := clusterOf[c.SourceNodeID]
		tgtK, ok2 := clusterOf[c.TargetNodeID]
		if !ok1 || !ok2 {
			t.Fatalf("candidate references unknown node: %+v", c)
		}
		pairs = append(pairs, Pair{TruePositive: srcK == tgtK})
	}
	fp, tp := FPCounts(pairs)
	fpRate := FPRate(pairs)

	// emit JSON + single-line summary
	res := Result{
		Population:          pop,
		Clusters:            clusters,
		NodesPerCluster:     nodesPerCluster,
		CandidatesPerSource: topK,
		Threshold:           threshold,
		FPRate:              fpRate,
		FP:                  fp,
		TP:                  tp,
		TotalCandidates:     len(pairs),
		Embedder:            embedderName,
		Backend:             backendName,
		Timestamp:           time.Now().UTC(),
	}
	if err := WriteJSON("autolink_fp_results.json", res); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	fmt.Printf("AUTOLINK_FP pop=%d fp_rate=%.5f tp=%d fp=%d total=%d threshold=%.2f backend=%s\n",
		pop, fpRate, tp, fp, len(pairs), threshold, backendName)
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

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

func contentHashFor(nodeID string) string { return "h-" + nodeID }

// seedNodes inserts a synthetic-corpus batch into the nodes table.
func seedNodes(t *testing.T, db *sql.DB, repoID, branch string, nodes []synthcorpus.SyntheticNode) {
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
			1, 1, contentHashFor(n.NodeID), now, "autolink-eval", "system",
		); err != nil {
			t.Fatalf("insert node %s: %v", n.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedEmbeddings populates node_embeddings (the content-addressed
// bytes table) and node_embedding_refs (state='ready') so that
// EmbeddingRefRepo.ContentHashForNode and LookupExisting return real
// rows. The Linker calls both for every source node. vecOf supplies
// the vector for node index i - either from a shared on-disk fixture
// or from FakeEmbed.
func seedEmbeddings(t *testing.T, db *sql.DB, nodes []synthcorpus.SyntheticNode, vecOf func(int, synthcorpus.SyntheticNode) []float32) {
	t.Helper()
	now := time.Now().UnixMilli()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	embedStmt, err := tx.Prepare(`INSERT OR IGNORE INTO node_embeddings
		(content_hash, model, dim, embedding, created_at) VALUES (?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare embed: %v", err)
	}
	defer embedStmt.Close()

	refStmt, err := tx.Prepare(`INSERT INTO node_embedding_refs
		(node_id, content_hash, state, enqueued_at, embedded_at)
		VALUES (?, ?, 'ready', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare ref: %v", err)
	}
	defer refStmt.Close()

	for i, n := range nodes {
		hash := contentHashFor(n.NodeID)
		vec := vecOf(i, n)
		blob := encodeF32LE(vec)
		if _, err := embedStmt.Exec(hash, "fake-hash-v1", len(vec), blob, now); err != nil {
			t.Fatalf("insert node_embeddings %s: %v", n.NodeID, err)
		}
		if _, err := refStmt.Exec(n.NodeID, hash, now, now); err != nil {
			t.Fatalf("insert node_embedding_refs %s: %v", n.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// loadEmbeddings resolves the per-node vector source. If the shared
// recall-harness fixture for the given population exists, it's replayed
// (Ollama-seeded vectors flow into both harnesses); otherwise the
// deterministic FakeEmbed path is used. The returned vecOf is the
// per-node accessor used at the upsert + seed sites.
func loadEmbeddings(t *testing.T, nodes []synthcorpus.SyntheticNode, pop int, fallbackName string, semantic bool) (func(int, synthcorpus.SyntheticNode) []float32, string) {
	t.Helper()
	fixturePath := recall.FixturePath(sharedFixtureDir, pop)
	if semantic {
		fixturePath = filepath.Join(sharedFixtureDir, fmt.Sprintf("embeddings_semantic_%d.bin", pop))
	}
	if _, err := os.Stat(fixturePath); err != nil {
		return func(_ int, n synthcorpus.SyntheticNode) []float32 {
			return synthcorpus.FakeEmbed(n.Text)
		}, fallbackName
	}
	dim, vecs, err := recall.ReadFixture(fixturePath)
	if err != nil {
		t.Fatalf("autolink: ReadFixture(%s): %v", fixturePath, err)
	}
	if got := len(vecs) / dim; got != len(nodes) {
		t.Fatalf("autolink: shared fixture %s holds %d vectors, expected %d", fixturePath, got, len(nodes))
	}
	t.Logf("autolink: replaying %d vectors (dim=%d) from shared fixture %s", len(nodes), dim, fixturePath)
	return func(i int, _ synthcorpus.SyntheticNode) []float32 {
		out := make([]float32, dim)
		copy(out, vecs[i*dim:(i+1)*dim])
		return out
	}, "fixture"
}

// encodeF32LE mirrors application/embedder.encodeFloat32LE - duplicated
// in the harness to keep tools/ free of an upward import into a
// non-exported helper.
func encodeF32LE(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}
