// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval && hnsw_native

// Package usearchab is a manual A/B harness that compares the usearch (HNSW,
// float16) vector backend against the exact memvec linear-scan backend on the
// REAL Veska graph, using the production node_embeddings already persisted in
// the live veska.db.
//
// memvec is the oracle: its linear scan returns exact top-k, so memvec's
// autolink candidate set is ground truth. The comparison is therefore a
// DIRECTIONAL diff of the autolink edge sets each backend produces from
// byte-identical vectors:
//
//   - memvec-only edges = links usearch MISSED        -> recall cost (HNSW approximation)
//   - usearch-only edges = links only usearch found   -> precision noise (float16 quantization)
//   - shared            = agreement
//
// Embedding is removed as a variable entirely (the same persisted vectors feed
// both stores), so the index is the only thing that differs.
//
// Run (requires libusearch_c.so on the loader path and a populated ~/.veska):
//
//	go test -tags "eval hnsw_native sqlite_fts5" -run TestUsearchAB ./tools/loadtest/usearchab/ -v -timeout 30m
//
// Honest scoping: at the live graph's ~13k nodes this answers QUALITY
// (recall/precision), not latency - HNSW's speed win lives above the crossover.
// The reported p50/p95 is informational; expect memvec to be competitive here.
package usearchab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// bucketKey scopes embeddings (and the autolink candidate run) to one
// (repo, branch) pair - the same partitioning VectorStorage uses.
type bucketKey struct{ repo, branch string }

// nodeVec is one source node's persisted vector, carried alongside its ModelID
// so it lands in the right usearch partition.
type nodeVec struct {
	nodeID  string
	hash    string
	modelID string
	vec     []float32
}

// edge is a directed autolink candidate (source -> target). The score is the
// emitting backend's score; it is intentionally excluded from the set key so
// the diff compares the EDGE relation, with float16 score drift reported
// separately on the shared edges.
type edge struct{ src, tgt string }

func TestUsearchAB(t *testing.T) {
	ctx := context.Background()

	srcDB := liveDBPath()
	if _, err := os.Stat(srcDB); err != nil {
		t.Skipf("no live veska.db at %s (set VESKA_HOME); nothing to compare", srcDB)
	}

	// Copy the live db (+ WAL/SHM) into the test tempdir so this harness cannot
	// perturb a running daemon's open handle. 120MB copy is trivial and buys
	// full isolation from the live writer.
	dbPath := filepath.Join(t.TempDir(), "veska.db")
	copyDBFiles(t, srcDB, dbPath)

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("OpenPools(%s): %v", dbPath, err)
	}
	defer func() { _ = pools.ReadDB.Close(); _ = pools.Write.Close() }()

	archive := sqlite.NewEmbeddingArchive(pools.ReadDB, pools.Write)
	rows, err := archive.LoadReadyEmbeddings(ctx)
	if err != nil {
		t.Fatalf("LoadReadyEmbeddings: %v", err)
	}
	if len(rows) == 0 {
		t.Skip("live db has no ready embeddings")
	}

	// Decode persisted blobs into query vectors, bucketed per (repo, branch).
	buckets := make(map[bucketKey][]nodeVec)
	for _, r := range rows {
		vec := veccodec.DecodeFloat32LE(r.Blob, r.Dim)
		if len(vec) == 0 {
			continue
		}
		k := bucketKey{repo: r.RepoID, branch: r.Branch}
		buckets[k] = append(buckets[k], nodeVec{
			nodeID:  r.NodeID,
			hash:    r.ContentHash,
			modelID: r.ModelID,
			vec:     vec,
		})
	}

	names := loadSymbolNames(t, pools.ReadDB)

	// USEARCH_AB_SCALE>1 synthesizes a larger corpus by appending perturbed,
	// renormalized copies of the real vectors (new node IDs). This probes recall
	// parity, query latency, and index-BUILD cost at higher N while keeping the
	// real embedding distribution. Caveat: synthetic copies inflate absolute edge
	// counts (each gains a near-twin); the memvec-vs-usearch recall PARITY stays
	// valid (memvec is still the exact oracle), but absolute edge semantics do not.
	scale := envInt("USEARCH_AB_SCALE", 1)
	if scale > 1 {
		total := 0
		for k := range buckets {
			buckets[k] = expandBucket(buckets[k], scale)
			total += len(buckets[k])
		}
		t.Logf("USEARCH_AB_SCALE=%d -> synthesized corpus, %d total nodes", scale, total)
	}

	// Build both stores once and upsert byte-identical vectors. usearch needs a
	// dir for its on-disk index files; memvec ignores it.
	mem, err := vector.NewVectorStorage(vector.BackendMemory, t.TempDir())
	if err != nil {
		t.Fatalf("new memvec: %v", err)
	}
	use, err := vector.NewVectorStorage(vector.BackendUsearch, t.TempDir())
	if err != nil {
		t.Fatalf("new usearch (libusearch_c.so on path?): %v", err)
	}
	if d, ok := use.(interface{ Destroy() }); ok {
		defer d.Destroy()
	}

	memBuild := upsertAll(ctx, t, mem, buckets)
	useBuild := upsertAll(ctx, t, use, buckets)
	t.Logf("index BUILD (upsert all): memvec=%s  usearch=%s   (usearch includes HNSW graph construction; this is the cold-scan cost term)",
		memBuild.Round(time.Millisecond), useBuild.Round(time.Millisecond))

	report := abReport{
		DBPath:     srcDB,
		Scale:      scale,
		TopK:       autolink.DefaultTopK,
		Threshold:  autolink.DefaultThreshold,
		MemBuildMs: float64(memBuild.Microseconds()) / 1000.0,
		UseBuildMs: float64(useBuild.Microseconds()) / 1000.0,
	}

	for k, nvs := range buckets {
		memEdges, memScore, memLat := candidates(ctx, t, mem, k, nvs)
		useEdges, useScore, useLat := candidates(ctx, t, use, k, nvs)

		memOnly, useOnly, shared := diff(memEdges, useEdges)
		recall := 1.0
		if len(memEdges) > 0 {
			recall = float64(shared) / float64(len(memEdges))
		}

		br := bucketReport{
			Repo:        k.repo,
			Branch:      k.branch,
			Nodes:       len(nvs),
			MemEdges:    len(memEdges),
			UseEdges:    len(useEdges),
			Shared:      shared,
			MemOnly:     len(memOnly),
			UseOnly:     len(useOnly),
			Recall:      recall,
			MemP50ms:    pct(memLat, 50),
			MemP95ms:    pct(memLat, 95),
			UseP50ms:    pct(useLat, 50),
			UseP95ms:    pct(useLat, 95),
			MissSamples: sampleEdges(memOnly, memScore, names, 25),
			ExtraSample: sampleEdges(useOnly, useScore, names, 25),
		}
		report.Buckets = append(report.Buckets, br)

		t.Logf("\n=== %s @ %s : %d nodes ===", k.repo, k.branch, len(nvs))
		t.Logf("memvec edges=%d  usearch edges=%d  shared=%d", len(memEdges), len(useEdges), shared)
		t.Logf("RECALL (usearch vs exact memvec) = %.4f  (%d/%d)", recall, shared, len(memEdges))
		t.Logf("memvec-only (MISSED by usearch)  = %d   <- recall cost", len(memOnly))
		t.Logf("usearch-only (float16 precision) = %d   <- not in exact top-k", len(useOnly))
		t.Logf("latency p50/p95 ms  memvec=%.3f/%.3f  usearch=%.3f/%.3f",
			br.MemP50ms, br.MemP95ms, br.UseP50ms, br.UseP95ms)
		logSamples(t, "MISSED by usearch (memvec-only)", br.MissSamples)
		logSamples(t, "usearch-only (float16 noise)", br.ExtraSample)
	}

	out := filepath.Join(os.TempDir(), "usearch-ab.json")
	writeJSON(t, out, report)
	t.Logf("\nfull report: %s", out)
}

// candidates mirrors autolink.Linker.Candidates exactly: Search(vec, k+1) to
// leave room for the self-hit, drop the self-hit and sub-threshold hits, emit
// at most k. Returns the directed edge set, per-edge scores, and per-query
// Search latencies.
func candidates(ctx context.Context, t *testing.T, store ports.VectorStorage, k bucketKey, nvs []nodeVec) (map[edge]struct{}, map[edge]float32, []time.Duration) {
	edges := make(map[edge]struct{}, len(nvs)*autolink.DefaultTopK)
	scores := make(map[edge]float32, len(nvs)*autolink.DefaultTopK)
	lat := make([]time.Duration, 0, len(nvs))

	for _, nv := range nvs {
		start := time.Now()
		hits, err := store.Search(ctx, k.repo, k.branch, nv.vec, autolink.DefaultTopK+1, domain.VectorFilter{})
		lat = append(lat, time.Since(start))
		if err != nil {
			t.Fatalf("Search %s: %v", nv.nodeID, err)
		}
		emitted := 0
		for _, h := range hits {
			if emitted >= autolink.DefaultTopK {
				break
			}
			if h.NodeID == nv.nodeID {
				continue
			}
			if h.Score < autolink.DefaultThreshold {
				continue
			}
			e := edge{src: nv.nodeID, tgt: h.NodeID}
			edges[e] = struct{}{}
			scores[e] = h.Score
			emitted++
		}
	}
	return edges, scores, lat
}

// diff splits the two edge sets into memvec-only (recall cost), usearch-only
// (precision noise), and a shared count.
func diff(mem, use map[edge]struct{}) (memOnly, useOnly []edge, shared int) {
	for e := range mem {
		if _, ok := use[e]; ok {
			shared++
		} else {
			memOnly = append(memOnly, e)
		}
	}
	for e := range use {
		if _, ok := mem[e]; !ok {
			useOnly = append(useOnly, e)
		}
	}
	return memOnly, useOnly, shared
}

// upsertAll loads every bucket into the store and returns the total wall time,
// which for usearch includes HNSW graph construction - the index-build cost that
// shows up in cold-scan time (as opposed to the per-query latency measured later).
func upsertAll(ctx context.Context, t *testing.T, store ports.VectorStorage, buckets map[bucketKey][]nodeVec) time.Duration {
	start := time.Now()
	for k, nvs := range buckets {
		batch := make([]domain.EmbeddingRow, 0, len(nvs))
		for _, nv := range nvs {
			batch = append(batch, domain.EmbeddingRow{
				NodeID:      nv.nodeID,
				ContentHash: nv.hash,
				ModelID:     nv.modelID,
				Vector:      nv.vec,
			})
		}
		if err := store.UpsertEmbeddings(ctx, k.repo, k.branch, batch); err != nil {
			t.Fatalf("upsert %s/%s: %v", k.repo, k.branch, err)
		}
	}
	return time.Since(start)
}

// expandBucket returns the real vectors plus (scale-1) perturbed, renormalized
// copies of each, with derived node IDs. The perturbation is large enough to
// avoid degenerate exact-duplicate ties yet keeps copies in-distribution
// (unit-L2, matching the production L2-normalized embeddings). Seeded for
// reproducibility.
func expandBucket(nvs []nodeVec, scale int) []nodeVec {
	r := rand.New(rand.NewSource(0xC0FFEE))
	out := make([]nodeVec, 0, len(nvs)*scale)
	out = append(out, nvs...)
	for c := 1; c < scale; c++ {
		for _, nv := range nvs {
			out = append(out, nodeVec{
				nodeID:  nv.nodeID + "#dup" + strconv.Itoa(c),
				hash:    nv.hash + "#dup" + strconv.Itoa(c),
				modelID: nv.modelID,
				vec:     perturb(r, nv.vec),
			})
		}
	}
	return out
}

// perturb adds Gaussian noise to a copy of vec and renormalizes to unit L2.
// noiseStd ~0.015/component over ~768 dims lands copies in the "related"
// similarity band rather than as near-exact twins.
func perturb(r *rand.Rand, vec []float32) []float32 {
	const noiseStd = 0.015
	out := make([]float32, len(vec))
	var sum float64
	for i, v := range vec {
		nv := float64(v) + r.NormFloat64()*noiseStd
		out[i] = float32(nv)
		sum += nv * nv
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return out
	}
	for i := range out {
		out[i] = float32(float64(out[i]) / norm)
	}
	return out
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// --- reporting ---------------------------------------------------------------

type abReport struct {
	DBPath     string         `json:"db_path"`
	Scale      int            `json:"scale"`
	TopK       int            `json:"top_k"`
	Threshold  float32        `json:"threshold"`
	MemBuildMs float64        `json:"mem_build_ms"`
	UseBuildMs float64        `json:"use_build_ms"`
	Buckets    []bucketReport `json:"buckets"`
}

type bucketReport struct {
	Repo        string       `json:"repo"`
	Branch      string       `json:"branch"`
	Nodes       int          `json:"nodes"`
	MemEdges    int          `json:"mem_edges"`
	UseEdges    int          `json:"use_edges"`
	Shared      int          `json:"shared"`
	MemOnly     int          `json:"mem_only_missed"`
	UseOnly     int          `json:"use_only_precision_noise"`
	Recall      float64      `json:"recall"`
	MemP50ms    float64      `json:"mem_p50_ms"`
	MemP95ms    float64      `json:"mem_p95_ms"`
	UseP50ms    float64      `json:"use_p50_ms"`
	UseP95ms    float64      `json:"use_p95_ms"`
	MissSamples []edgeSample `json:"missed_samples"`
	ExtraSample []edgeSample `json:"extra_samples"`
}

type edgeSample struct {
	Src   string  `json:"src"`
	Tgt   string  `json:"tgt"`
	Score float32 `json:"score"`
}

func sampleEdges(edges []edge, scores map[edge]float32, names map[string]string, n int) []edgeSample {
	// Highest-scoring disagreements first - those are the most decision-relevant
	// (a missed link at score 0.95 matters more than one at 0.61).
	sort.Slice(edges, func(i, j int) bool { return scores[edges[i]] > scores[edges[j]] })
	if len(edges) > n {
		edges = edges[:n]
	}
	out := make([]edgeSample, 0, len(edges))
	for _, e := range edges {
		out = append(out, edgeSample{
			Src:   nameOr(names, e.src),
			Tgt:   nameOr(names, e.tgt),
			Score: scores[e],
		})
	}
	return out
}

func logSamples(t *testing.T, title string, s []edgeSample) {
	if len(s) == 0 {
		return
	}
	t.Logf("  -- %s (top %d by score) --", title, len(s))
	for _, e := range s {
		t.Logf("    %.4f  %s  ->  %s", e.Score, e.Src, e.Tgt)
	}
}

func nameOr(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return id
}

// loadSymbolNames maps node_id -> "symbol_path (file_path)" so the disagreeing
// edges are judgeable by someone who knows the code, not opaque ids.
func loadSymbolNames(t *testing.T, db *sql.DB) map[string]string {
	rows, err := db.Query(`SELECT node_id, symbol_path, file_path FROM nodes`)
	if err != nil {
		t.Fatalf("load symbol names: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string, 16384)
	for rows.Next() {
		var id, sym, file string
		if err := rows.Scan(&id, &sym, &file); err != nil {
			t.Fatalf("scan node: %v", err)
		}
		out[id] = fmt.Sprintf("%s (%s)", sym, file)
	}
	return out
}

func pct(durs []time.Duration, p int) float64 {
	if len(durs) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), durs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p * (len(s) - 1)) / 100
	return float64(s[idx].Microseconds()) / 1000.0
}

func liveDBPath() string {
	if h := os.Getenv("VESKA_HOME"); h != "" {
		return filepath.Join(h, "veska.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".veska", "veska.db")
}

func copyDBFiles(t *testing.T, src, dst string) {
	// Copy the main db plus any WAL/SHM sidecars so the snapshot is consistent
	// with whatever the live writer has not yet checkpointed.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		s := src + suffix
		if _, err := os.Stat(s); err != nil {
			continue
		}
		copyFile(t, s, dst+suffix)
	}
}

func copyFile(t *testing.T, src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
}
