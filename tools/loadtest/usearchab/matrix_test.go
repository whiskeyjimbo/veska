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
// usearch's index lives C-side via cgo, so it's read from usearch's own
// MemoryUsage() (HeapAlloc can't see it and RSS deltas are unreliable - glibc
// retains freed C memory across buckets). Both reported in MiB.
package usearchab

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
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
	IndexSecs   int     `json:"index_secs"` // one-time parse+embed cost (backend-independent), 0 if unknown
	MemBuildMs  float64 `json:"mem_build_ms"`
	UseBuildMs  float64 `json:"use_build_ms"`
	MemP50ms    float64 `json:"mem_p50_ms"`
	MemP95ms    float64 `json:"mem_p95_ms"`
	UseP50ms    float64 `json:"use_p50_ms"`
	UseP95ms    float64 `json:"use_p95_ms"`
	Recall      float64 `json:"recall"` // default profile, vs memvec oracle; -1 = n/a
	MemMiB      float64 `json:"mem_mib"`
	UseMiB      float64 `json:"use_mib"`
	AutolinkEdg int     `json:"autolink_edges"`

	// Profiles holds the build-time-vs-recall measurement for each usearch
	// build profile. Query latency/RAM above are profile-independent (the search
	// beam is fixed), so they are captured once from the default profile.
	Profiles []profileMeasure `json:"profiles"`
}

// profileMeasure is one usearch_index_profile's build cost and recall. Parallel
// profiles are nondeterministic, so build/recall are the median over repeats with
// the observed min/max retained.
type profileMeasure struct {
	Name       string  `json:"name"`
	Parallel   bool    `json:"parallel"`
	BuildMs    float64 `json:"build_ms"`
	BuildMinMs float64 `json:"build_min_ms"`
	BuildMaxMs float64 `json:"build_max_ms"`
	Recall     float64 `json:"recall"` // -1 = n/a (memvec oracle skipped)
	RecallMin  float64 `json:"recall_min"`
	RecallMax  float64 `json:"recall_max"`
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
	buckets := loadBucketsForMatrix(t, ctx, srcDB)
	if len(buckets) == 0 {
		t.Skip("no ready embeddings in db")
	}

	// Repo-id -> short label / tier, optional, via USEARCH_AB_LABELS="repoid=S:go-git,repoid2=L:consul".
	labels := parseLabels(os.Getenv("USEARCH_AB_LABELS"))
	times := parseIntMap(os.Getenv("USEARCH_AB_INDEX_TIMES"))
	// USEARCH_AB_MAX_QUERIES caps the per-node autolink sweep to a random sample
	// (build/RAM still use the full index) so O(n^2) memvec stays tractable on big
	// repos; 0 = every node. USEARCH_AB_MEMVEC_MAX_NODES skips the memvec build for
	// buckets above it (usearch-only row, recall n/a) when an in-RAM exact index is
	// impractical; 0 = always build memvec.
	maxQueries := envInt("USEARCH_AB_MAX_QUERIES", 0)
	memvecMax := envInt("USEARCH_AB_MEMVEC_MAX_NODES", 0)
	// Parallel build profiles are nondeterministic; each runs this many times and
	// reports median/min/max. Serial profiles (default, accurate) run once.
	repeats := envInt("USEARCH_MATRIX_REPEATS", 3)

	var rows []matrixRow
	for k, nvs := range buckets {
		bs := time.Now()
		row := measureBucket(t, ctx, k, nvs, labels, maxQueries, repeats, memvecMax > 0 && len(nvs) > memvecMax)
		row.IndexSecs = times[k.repo]
		rows = append(rows, row)
		t.Logf("bucket %s (%d nodes) measured in %s", row.Repo, row.Nodes, time.Since(bs).Round(time.Second))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Nodes < rows[j].Nodes })

	table := renderMatrix(rows) + "\n" + renderProfileTable(rows) + matrixGlossary
	fmt.Print("\n" + table + "\n")
	out := "/tmp/backend-matrix.md"
	if err := os.WriteFile(out, []byte(table), 0o644); err != nil {
		t.Fatalf("write table: %v", err)
	}
	t.Logf("matrix written to %s", out)
}

// measureBucket builds memvec and/or usearch in isolation for one bucket and
// measures build time, memory, autolink recall (memvec = oracle), and per-query
// latency. Both stores are always built from the FULL vector set; recall and
// latency are measured over a query SAMPLE (queryNodes) so the O(n^2) autolink
// sweep stays tractable on large repos. When memvecSkip is set (bucket too big
// for an in-RAM exact index) memvec is skipped entirely - a usearch-only row,
// recall reported as n/a.
func measureBucket(t *testing.T, ctx context.Context, k bucketKey, nvs []nodeVec, labels map[string]string, maxQueries, repeats int, memvecSkip bool) matrixRow {
	batch := make([]domain.EmbeddingRow, len(nvs))
	for i, nv := range nvs {
		batch[i] = domain.EmbeddingRow{NodeID: nv.nodeID, ContentHash: nv.hash, ModelID: nv.modelID, Vector: nv.vec}
	}
	queryNodes := sampleNodes(nvs, maxQueries)

	label := labels[k.repo]
	if label == "" {
		label = shortRepo(k.repo)
	}
	row := matrixRow{Repo: label, Branch: k.branch, Nodes: len(nvs), Recall: -1} // -1 = n/a

	// --- memvec oracle (unless skipped): exact edges + query latency + RAM ---
	// Built first so the usearch profiles below can score recall against it.
	var memEdges map[edge]struct{}
	if !memvecSkip {
		mem, err := vector.NewVectorStorage(vector.BackendMemory, t.TempDir())
		if err != nil {
			t.Fatalf("new memvec: %v", err)
		}
		heap0 := heapAllocBytes()
		memStart := time.Now()
		if err := mem.UpsertEmbeddings(ctx, k.repo, k.branch, batch); err != nil {
			t.Fatalf("memvec upsert: %v", err)
		}
		row.MemBuildMs = msOf(time.Since(memStart))
		row.MemMiB = mibSince(heap0, heapAllocBytes())
		var memLat []time.Duration
		memEdges, _, memLat = candidates(ctx, t, mem, k, queryNodes)
		row.MemP50ms, row.MemP95ms = pct(memLat, 50), pct(memLat, 95)
		row.AutolinkEdg = len(memEdges)
	}

	// --- usearch build-profile sweep ---
	// Profiles trade BUILD time vs RECALL only - the search beam (ef_search) is
	// fixed, so query latency + RAM are profile-independent and captured once
	// from the default profile. Parallel profiles are nondeterministic, so they
	// run `repeats` times and report median/min/max; serial profiles run once.
	profiles := []struct {
		name     string
		parallel bool
	}{
		{vector.ProfileDefault, false},
		{vector.ProfileFast, true},
		{vector.ProfileBalanced, true},
		{vector.ProfileAccurate, false},
	}
	for _, p := range profiles {
		reps := 1
		if p.parallel && repeats > 1 {
			reps = repeats
		}
		var builds, recalls []float64
		for i := 0; i < reps; i++ {
			opts, err := vector.OptionsForProfile(p.name)
			if err != nil {
				t.Fatalf("profile %q: %v", p.name, err)
			}
			use, err := vector.NewVectorStorage(vector.BackendUsearch, t.TempDir(), opts...)
			if err != nil {
				t.Fatalf("new usearch %q: %v", p.name, err)
			}
			start := time.Now()
			if err := use.UpsertEmbeddings(ctx, k.repo, k.branch, batch); err != nil {
				t.Fatalf("usearch upsert %q: %v", p.name, err)
			}
			buildMs := msOf(time.Since(start))
			builds = append(builds, buildMs)
			useEdges, _, useLat := candidates(ctx, t, use, k, queryNodes)
			if memEdges != nil {
				rc := 1.0
				if len(memEdges) > 0 {
					_, _, shared := diff(memEdges, useEdges)
					rc = float64(shared) / float64(len(memEdges))
				}
				recalls = append(recalls, rc)
			}
			// The default profile supplies the profile-independent figures.
			if p.name == vector.ProfileDefault && i == 0 {
				row.UseBuildMs = buildMs
				row.UseP50ms, row.UseP95ms = pct(useLat, 50), pct(useLat, 95)
				if mu, ok := use.(interface{ MemoryUsage() (uint64, error) }); ok {
					if b, err := mu.MemoryUsage(); err == nil {
						row.UseMiB = float64(b) / (1024 * 1024)
					}
				}
				if len(recalls) > 0 {
					row.Recall = recalls[0]
				}
			}
			if d, ok := use.(interface{ Destroy() }); ok {
				d.Destroy()
			}
		}
		pm := profileMeasure{Name: p.name, Parallel: p.parallel, Recall: -1}
		pm.BuildMs, pm.BuildMinMs, pm.BuildMaxMs = stats(builds)
		if len(recalls) > 0 {
			pm.Recall, pm.RecallMin, pm.RecallMax = stats(recalls)
		}
		row.Profiles = append(row.Profiles, pm)
	}
	return row
}

// stats returns the median, min, and max of xs (median = upper-middle for even
// counts). Empty input yields zeros.
func stats(xs []float64) (med, lo, hi float64) {
	if len(xs) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2], s[0], s[len(s)-1]
}

// sampleNodes returns up to max nodes (seeded random subset) for the query set;
// max <= 0 or a bucket already under the cap returns the whole set.
func sampleNodes(nvs []nodeVec, max int) []nodeVec {
	if max <= 0 || len(nvs) <= max {
		return nvs
	}
	r := rand.New(rand.NewSource(0x5A11D5))
	idx := r.Perm(len(nvs))[:max]
	out := make([]nodeVec, max)
	for i, j := range idx {
		out[i] = nvs[j]
	}
	return out
}

func renderMatrix(rows []matrixRow) string {
	var b strings.Builder
	b.WriteString("## memvec vs usearch - backend metrics matrix\n\n")
	b.WriteString("Backend comparison at the **default** build profile. This is the *which backend* axis: memvec's query latency grows with size (O(n) scan), usearch's stays flat (O(log n)) - the time-to-use trade. ")
	b.WriteString("Autolink recall is usearch vs the exact memvec oracle (memvec is 1.0000 by definition). ")
	b.WriteString("Build = time to construct the in-memory index from persisted vectors. ")
	b.WriteString("Query p50/p95 = per-node nearest-neighbour search latency (profile-independent). Memory = marginal index footprint.\n\n")
	b.WriteString("Index (parse+embed) is the one-time, backend-independent cost to make the repo searchable.\n\n")
	b.WriteString("| repo | nodes | index (parse+embed) | build mem | build usearch | q p50 mem | q p50 usearch | q p95 mem | q p95 usearch | usearch recall | mem RAM | usearch RAM |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, r := range rows {
		idx := "-"
		if r.IndexSecs > 0 {
			idx = fmt.Sprintf("%ds", r.IndexSecs)
		}
		// Recall < 0 means memvec was skipped (bucket too big for an in-RAM exact
		// index): a usearch-only row, memvec columns blanked.
		memBuild := dur(r.MemBuildMs)
		memP50 := fmt.Sprintf("%.2fms", r.MemP50ms)
		memP95 := fmt.Sprintf("%.2fms", r.MemP95ms)
		memRAM := fmt.Sprintf("%.0f MiB", r.MemMiB)
		recall := fmt.Sprintf("%.4f", r.Recall)
		if r.Recall < 0 {
			memBuild, memP50, memP95, memRAM, recall = "—", "—", "—", "—", "n/a"
		}
		b.WriteString(fmt.Sprintf("| %s | %d | %s | %s | %s | %s | %.2fms | %s | %.2fms | %s | %s | %.0f MiB |\n",
			r.Repo, r.Nodes, idx,
			memBuild, dur(r.UseBuildMs),
			memP50, r.UseP50ms, memP95, r.UseP95ms,
			recall, memRAM, r.UseMiB))
	}
	return b.String()
}

// renderProfileTable is the *which setting* axis: how each usearch_index_profile
// trades index build time against autolink recall. Query latency is omitted on
// purpose - it does not vary by profile (the search beam is fixed).
func renderProfileTable(rows []matrixRow) string {
	var b strings.Builder
	b.WriteString("## usearch build profiles - build time vs recall\n\n")
	b.WriteString("The `usearch_index_profile` lever (`default`|`fast`|`balanced`|`accurate`) trades index BUILD time against autolink RECALL; ")
	b.WriteString("it does **not** change query latency (the search beam is fixed). Each cell is `build / recall` (recall vs the exact memvec oracle, `n/a` when the bucket was too big to build an exact index). ")
	b.WriteString("Parallel profiles (`fast`, `balanced`) are nondeterministic - build/recall are the median over repeated runs. The trade only bites at scale; below ~40k nodes every profile is ~instant at ~0.997+ recall.\n\n")
	b.WriteString("| repo | nodes | default (serial ef64) | fast (parallel ef64) | balanced (parallel ef128) | accurate (serial ef192) |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|\n")
	for _, r := range rows {
		cell := map[string]string{}
		for _, p := range r.Profiles {
			rc := "n/a"
			if p.Recall >= 0 {
				rc = fmt.Sprintf("%.4f", p.Recall)
			}
			cell[p.Name] = fmt.Sprintf("%s / %s", dur(p.BuildMs), rc)
		}
		b.WriteString(fmt.Sprintf("| %s | %d | %s | %s | %s | %s |\n",
			r.Repo, r.Nodes,
			cell[vector.ProfileDefault], cell[vector.ProfileFast], cell[vector.ProfileBalanced], cell[vector.ProfileAccurate]))
	}
	return b.String()
}

// matrixGlossary defines the terms in the table so the output is self-documenting
// when pasted into the manual.
const matrixGlossary = `
### Definitions

- **Node** — the unit Veska indexes: a single code symbol (function, type, method) or a chunk of one. A repo's "size" here is its node count, not lines of code.
- **Embedding** — a 768-number vector capturing the *meaning* of a node's code (produced by the embedder, model2vec by default). Semantic search and auto-linking work by comparing these vectors. Computing them is the slow part of indexing.
- **Index (parse + embed)** / *cold scan* — the one-time cost to first make a repo searchable: parse every file into nodes, store them in the graph, then embed each node. Everything after is incremental (only changed files re-index).
- **Build** — constructing the in-memory vector index from already-computed embeddings (e.g. on daemon restart). For usearch this is HNSW graph construction (seconds); for memvec it's just loading vectors into RAM (milliseconds).
- **Query p50 / p95** — how long a single nearest-neighbour search takes (p50 = median, p95 = the slow 95th-percentile tail). This is what a user feels during semantic search.
- **Recall** — of the truly-closest matches, the fraction the approximate backend (usearch) actually returns, measured against memvec's exact results (the *oracle*). 1.0000 = identical to exact; 0.99 ≈ misses ~1 in 100, usually unnoticeable in practice.
- **memvec vs usearch** — the two vector backends. *memvec* = exact brute-force linear scan (simple, low RAM, but query time grows with size). *usearch* = approximate HNSW graph (fast at any size, more RAM, needs the native libusearch_c.so).
- **Autolink** — Veska's feature that draws SIMILAR_TO edges between semantically-close nodes. It runs one nearest-neighbour query per node, so it's the heaviest user of the vector backend and where recall differences surface.
`

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

// parseIntMap turns "repoid=40,repoid2=100" into repoid -> 40 (seconds).
func parseIntMap(s string) map[string]int {
	out := map[string]int{}
	for _, pair := range strings.Split(s, ",") {
		if i := strings.Index(pair, "="); i > 0 {
			if n, err := strconv.Atoi(pair[i+1:]); err == nil {
				out[pair[:i]] = n
			}
		}
	}
	return out
}

// loadBucketsForMatrix mirrors TestUsearchAB's db-load: copy the db, decode the
// persisted float32 vectors, bucket per (repo, branch).
func loadBucketsForMatrix(t *testing.T, ctx context.Context, srcDB string) map[bucketKey][]nodeVec {
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
	return buckets
}
