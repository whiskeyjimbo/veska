// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval && hnsw_native

// TestBackendMatrix builds a size-vs-backend metrics table (memvec vs usearch)
// across every (repo, branch) already indexed in a veska.db, for the manual.
// Unlike TestUsearchAB (one focused A/B with sample dumps), this processes each
// bucket in ISOLATION - fresh stores per bucket - so the per-repo memory numbers
// are clean, and emits a markdown table sorted by node count.
//
// Point it at a db built by tools/loadtest/usearchab/backend-matrix.sh (which
// indexes a configured slate of repos into one isolated home):
//
//	USEARCH_AB_DB=/tmp/veska-backend-matrix/home/veska.db \
//	  go test -tags "eval hnsw_native sqlite_fts5" -run TestBackendMatrix ./tools/loadtest/usearchab/ -v -count=1 -timeout 60m
//
// Memory: memvec lives in the Go heap (measured exactly via runtime HeapAlloc);
// usearch's index lives C-side via cgo (measured via process VmRSS delta around
// its build, after a GC settles the Go heap). Both are the marginal cost of
// holding that repo's index, reported in MiB.
package usearchab

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// matrixRow is one repo's measured comparison.
type matrixRow struct {
	Repo        string  `json:"repo"`
	Branch      string  `json:"branch"`
	Nodes       int     `json:"nodes"`
	MemBuildMs  float64 `json:"mem_build_ms"`
	UseBuildMs  float64 `json:"use_build_ms"`
	MemP50ms    float64 `json:"mem_p50_ms"`
	MemP95ms    float64 `json:"mem_p95_ms"`
	UseP50ms    float64 `json:"use_p50_ms"`
	UseP95ms    float64 `json:"use_p95_ms"`
	Recall      float64 `json:"recall"`
	MemMiB      float64 `json:"mem_mib"`
	UseMiB      float64 `json:"use_mib"`
	AutolinkEdg int     `json:"autolink_edges"`
}

func TestBackendMatrix(t *testing.T) {
	ctx := context.Background()

	srcDB := os.Getenv("USEARCH_AB_DB")
	if srcDB == "" {
		srcDB = liveDBPath()
	}
	if _, err := os.Stat(srcDB); err != nil {
		t.Skipf("no db at %s (set USEARCH_AB_DB)", srcDB)
	}

	// Reuse TestUsearchAB's loader (copy db, decode persisted vectors per bucket).
	buckets, names := loadBucketsForMatrix(t, ctx, srcDB)
	if len(buckets) == 0 {
		t.Skip("no ready embeddings in db")
	}

	// Repo-id -> short label / tier, optional, via USEARCH_AB_LABELS="repoid=S:go-git,repoid2=L:consul".
	labels := parseLabels(os.Getenv("USEARCH_AB_LABELS"))

	var rows []matrixRow
	for k, nvs := range buckets {
		rows = append(rows, measureBucket(t, ctx, k, nvs, names, labels))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Nodes < rows[j].Nodes })

	table := renderMatrix(rows)
	fmt.Print("\n" + table + "\n")
	out := "/tmp/backend-matrix.md"
	if err := os.WriteFile(out, []byte(table), 0o644); err != nil {
		t.Fatalf("write table: %v", err)
	}
	t.Logf("matrix written to %s", out)
}

// measureBucket builds memvec then usearch in isolation for one bucket, timing
// the build, measuring memory, and computing autolink recall (memvec = oracle).
func measureBucket(t *testing.T, ctx context.Context, k bucketKey, nvs []nodeVec, names map[string]string, labels map[string]string) matrixRow {
	batch := make([]domain.EmbeddingRow, len(nvs))
	for i, nv := range nvs {
		batch[i] = domain.EmbeddingRow{NodeID: nv.nodeID, ContentHash: nv.hash, ModelID: nv.modelID, Vector: nv.vec}
	}

	// --- memvec: Go-heap, measured exactly via HeapAlloc ---
	mem, err := vector.NewVectorStorage(vector.BackendMemory, t.TempDir())
	if err != nil {
		t.Fatalf("new memvec: %v", err)
	}
	heap0 := heapAllocBytes()
	memStart := time.Now()
	if err := mem.UpsertEmbeddings(ctx, k.repo, k.branch, batch); err != nil {
		t.Fatalf("memvec upsert: %v", err)
	}
	memBuild := time.Since(memStart)
	memMiB := mibSince(heap0, heapAllocBytes())

	// --- usearch: C-side, measured via VmRSS delta after a GC settles Go heap ---
	use, err := vector.NewVectorStorage(vector.BackendUsearch, t.TempDir())
	if err != nil {
		t.Fatalf("new usearch: %v", err)
	}
	useStart := time.Now()
	if err := use.UpsertEmbeddings(ctx, k.repo, k.branch, batch); err != nil {
		t.Fatalf("usearch upsert: %v", err)
	}
	useBuild := time.Since(useStart)
	// usearch's index is C-side (cgo) - HeapAlloc and process RSS can't isolate it
	// (glibc retains freed C memory across buckets). usearch reports its own
	// footprint (float32 vectors + HNSW graph), which is the honest number.
	useMiB := 0.0
	if mu, ok := use.(interface{ MemoryUsage() (uint64, error) }); ok {
		if b, err := mu.MemoryUsage(); err == nil {
			useMiB = float64(b) / (1024 * 1024)
		}
	}

	memEdges, _, memLat := candidates(ctx, t, mem, k, nvs)
	useEdges, _, useLat := candidates(ctx, t, use, k, nvs)
	_, _, shared := diff(memEdges, useEdges)
	recall := 1.0
	if len(memEdges) > 0 {
		recall = float64(shared) / float64(len(memEdges))
	}

	if d, ok := use.(interface{ Destroy() }); ok {
		d.Destroy()
	}

	label := labels[k.repo]
	if label == "" {
		label = shortRepo(k.repo)
	}
	return matrixRow{
		Repo: label, Branch: k.branch, Nodes: len(nvs),
		MemBuildMs: msOf(memBuild), UseBuildMs: msOf(useBuild),
		MemP50ms: pct(memLat, 50), MemP95ms: pct(memLat, 95),
		UseP50ms: pct(useLat, 50), UseP95ms: pct(useLat, 95),
		Recall: recall, MemMiB: memMiB, UseMiB: useMiB,
		AutolinkEdg: len(memEdges),
	}
}

func renderMatrix(rows []matrixRow) string {
	var b strings.Builder
	b.WriteString("## memvec vs usearch - backend metrics matrix\n\n")
	b.WriteString("Autolink recall is usearch vs the exact memvec oracle (memvec is 1.0000 by definition). ")
	b.WriteString("Build = time to construct the in-memory index from persisted vectors. ")
	b.WriteString("Query p50/p95 = per-node nearest-neighbour search latency. Memory = marginal index footprint.\n\n")
	b.WriteString("| repo | nodes | build mem | build usearch | q p50 mem | q p50 usearch | q p95 mem | q p95 usearch | usearch recall | mem RAM | usearch RAM |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("| %s | %d | %s | %s | %.2fms | %.2fms | %.2fms | %.2fms | %.4f | %.0f MiB | %.0f MiB |\n",
			r.Repo, r.Nodes,
			dur(r.MemBuildMs), dur(r.UseBuildMs),
			r.MemP50ms, r.UseP50ms, r.MemP95ms, r.UseP95ms,
			r.Recall, r.MemMiB, r.UseMiB))
	}
	return b.String()
}

// --- memory + small helpers ---

func heapAllocBytes() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func mibSince(before, after uint64) float64 {
	if after < before {
		return 0
	}
	return float64(after-before) / (1024 * 1024)
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

func dur(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

func shortRepo(repoID string) string {
	if len(repoID) > 12 {
		return repoID[:12]
	}
	return repoID
}

// parseLabels turns "repoid=S:go-git,repoid2=L:consul" into repoid -> "S:go-git".
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if i := strings.Index(pair, "="); i > 0 {
			out[pair[:i]] = pair[i+1:]
		}
	}
	return out
}

// loadBucketsForMatrix mirrors TestUsearchAB's db-load: copy the db, decode the
// persisted float32 vectors, bucket per (repo, branch), plus the symbol names.
func loadBucketsForMatrix(t *testing.T, ctx context.Context, srcDB string) (map[bucketKey][]nodeVec, map[string]string) {
	dbPath := t.TempDir() + "/veska.db"
	copyDBFiles(t, srcDB, dbPath)
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	defer func() { _ = pools.ReadDB.Close(); _ = pools.Write.Close() }()

	archive := sqlite.NewEmbeddingArchive(pools.ReadDB, pools.Write)
	rows, err := archive.LoadReadyEmbeddings(ctx)
	if err != nil {
		t.Fatalf("LoadReadyEmbeddings: %v", err)
	}
	buckets := make(map[bucketKey][]nodeVec)
	for _, r := range rows {
		vec := veccodec.DecodeFloat32LE(r.Blob, r.Dim)
		if len(vec) == 0 {
			continue
		}
		k := bucketKey{repo: r.RepoID, branch: r.Branch}
		buckets[k] = append(buckets[k], nodeVec{nodeID: r.NodeID, hash: r.ContentHash, modelID: r.ModelID, vec: vec})
	}
	return buckets, loadSymbolNames(t, pools.ReadDB)
}
