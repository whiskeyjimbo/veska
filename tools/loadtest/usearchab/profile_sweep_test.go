// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval && hnsw_native

// TestProfileSweep calibrates the usearch build profiles (the speed-vs-accuracy
// lever) on the REAL graph. It sweeps a grid of build parallelism x construction
// beam (ef_construction) and reports, per cell, index BUILD time and autolink
// recall measured against the exact memvec oracle - the same byte-identical
// vectors feed every cell, so the index build is the only variable.
//
// Two properties make this the gate for whether parallel build ships:
//
//   - Parallel insertion is NONDETERMINISTIC: the HNSW graph differs run to run.
//     The decision signal is recall VARIANCE across repeats, not the mean - a
//     high mean with a wide min/max swing is a no-ship. Each parallel cell is
//     therefore run USEARCH_PROFILE_REPEATS times (default 3) and reported as
//     recall min/median/max. The serial cell is deterministic (one run).
//
//   - Recall must be measured on REAL embeddings: random vectors mask the exact
//     regression this exists to catch (parallel@ef64 measured 0.9992 -> 0.9967
//     on the real graph while showing parity on random vectors). The corpus is
//     the production node_embeddings, fed through the production UsearchStore via
//     vector.NewVectorStorage + the build Options - no bespoke index, so the
//     measured recall IS what production would produce at that setting.
//
// Only buckets with >= USEARCH_PROFILE_MIN_NODES nodes (default = parallelAddMin)
// are measured: below that the parallel build falls back to serial, so including
// small buckets would contaminate a "parallel" cell with serial measurements.
// Skipped buckets are logged.
//
// Run (needs libusearch_c.so on the loader path and a populated db):
//
//	USEARCH_AB_DB=/tmp/veska-backend-matrix/home/veska.db \
//	  go test -tags "eval hnsw_native sqlite_fts5" -run TestProfileSweep \
//	  ./tools/loadtest/usearchab/ -v -count=1 -timeout 60m
//
// USEARCH_AB_SCALE>1 (read by the loader) synthesizes a larger corpus from
// perturbed real vectors to probe the large-N regime where the lever matters.
package usearchab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// profileCell is one (threads, ef) point in the sweep grid.
type profileCell struct {
	threads uint
	ef      uint
	repeats int
}

func (c profileCell) label() string {
	mode := fmt.Sprintf("par x%d", c.threads)
	if c.threads <= 1 {
		mode = "serial"
	}
	return fmt.Sprintf("%s / ef%d", mode, c.ef)
}

// cellResult accumulates recall and build-time samples across the repeats of one
// (grid cell, bucket) pair. Results are reported PER BUCKET, never aggregated
// across buckets: a 618-node bucket and a 12687-node bucket swing very
// differently, and a cross-bucket min/median would conflate them into a number
// no decision can be read off (small buckets dominate the min, masking the
// large-bucket signal that actually matters).
type cellResult struct {
	cell      profileCell
	repo      string
	branch    string
	nodes     int
	recalls   []float64
	buildSecs []float64
}

func TestProfileSweep(t *testing.T) {
	ctx := context.Background()

	srcDB := os.Getenv("USEARCH_AB_DB")
	if srcDB == "" {
		srcDB = liveDBPath()
	}
	if _, err := os.Stat(srcDB); err != nil {
		t.Skipf("no db at %s (set USEARCH_AB_DB)", srcDB)
	}

	buckets := loadBucketsForMatrix(t, ctx, srcDB)
	if len(buckets) == 0 {
		t.Skip("no ready embeddings in db")
	}

	minNodes := envInt("USEARCH_PROFILE_MIN_NODES", 256) // = parallelAddMin
	repeats := envInt("USEARCH_PROFILE_REPEATS", 3)
	// maxQueries caps the per-node autolink recall sweep to a deterministic random
	// SAMPLE so the O(n) query pass stays tractable on large buckets (188k x many
	// cells x repeats is otherwise multi-hour). The index is always BUILT from the
	// full corpus - only the recall/latency query set is sampled - and the oracle
	// uses the identical sample, so recall stays valid. 0 = every node.
	maxQueries := envInt("USEARCH_PROFILE_MAX_QUERIES", 0)

	// Keep only buckets big enough that the parallel build actually fans out;
	// smaller ones fall back to serial and would not measure the parallel path.
	big := make(map[bucketKey][]nodeVec)
	for k, nvs := range buckets {
		if len(nvs) >= minNodes {
			big[k] = nvs
		} else {
			t.Logf("skip bucket %s@%s (%d nodes < %d min) - parallel build is serial below the floor", k.repo, k.branch, len(nvs), minNodes)
		}
	}
	if len(big) == 0 {
		t.Skipf("no bucket has >= %d nodes; raise USEARCH_AB_SCALE or lower USEARCH_PROFILE_MIN_NODES", minNodes)
	}

	// memvec oracle per bucket: exact top-k, so its autolink edge set is ground
	// truth. Built once; every usearch cell is scored against it.
	// queries[k] is the (sampled) query set used for BOTH the oracle and every
	// usearch cell on bucket k, so recall compares like with like. Built once,
	// deterministic.
	queries := make(map[bucketKey][]nodeVec, len(big))
	oracle := make(map[bucketKey]map[edge]struct{}, len(big))
	for k, nvs := range big {
		q := sampleNodes(nvs, maxQueries)
		queries[k] = q
		mem, err := vector.NewVectorStorage(vector.BackendMemory, t.TempDir())
		if err != nil {
			t.Fatalf("new memvec: %v", err)
		}
		upsertOne(ctx, t, mem, k, nvs)
		memEdges, _, _ := candidates(ctx, t, mem, k, q)
		oracle[k] = memEdges
		t.Logf("oracle bucket %s@%s: %d nodes, %d queries, %d autolink edges", k.repo, k.branch, len(nvs), len(q), len(memEdges))
	}

	cores := runtime.GOMAXPROCS(0)
	results := runSweep(ctx, t, sweepGrid(cores, repeats), big, queries, oracle)

	table := renderProfileSweep(results, cores, minNodes, repeats)
	fmt.Print("\n" + table + "\n")
	out := "/tmp/usearch-profile-sweep.md"
	if err := os.WriteFile(out, []byte(table), 0o644); err != nil {
		t.Fatalf("write table: %v", err)
	}
	writeProfileJSON(t, "/tmp/usearch-profile-sweep.json", results, cores)
	t.Logf("profile sweep written to %s (+ .json)", out)
}

// jsonRow is one (cell, bucket) result flattened for the grapher.
type jsonRow struct {
	Profile      string  `json:"profile"`
	Threads      uint    `json:"threads"`
	EF           uint    `json:"ef"`
	Repo         string  `json:"repo"`
	Branch       string  `json:"branch"`
	Nodes        int     `json:"nodes"`
	Runs         int     `json:"runs"`
	RecallMin    float64 `json:"recall_min"`
	RecallMed    float64 `json:"recall_med"`
	RecallMax    float64 `json:"recall_max"`
	BuildSecsMin float64 `json:"build_secs_min"`
	BuildSecsMed float64 `json:"build_secs_med"`
	BuildSecsMax float64 `json:"build_secs_max"`
}

// writeProfileJSON emits the per-(cell,bucket) results as JSON so the curve can
// be graphed (build-time-vs-N and recall-vs-N per profile) without re-parsing
// the markdown table.
func writeProfileJSON(t *testing.T, path string, results []cellResult, cores int) {
	rows := make([]jsonRow, 0, len(results))
	for _, r := range results {
		mnR, mdR, mxR := minMedMax(r.recalls)
		mnB, mdB, mxB := minMedMax(r.buildSecs)
		rows = append(rows, jsonRow{
			Profile: r.cell.label(), Threads: r.cell.threads, EF: r.cell.ef,
			Repo: shortRepo(r.repo), Branch: r.branch, Nodes: r.nodes, Runs: len(r.recalls),
			RecallMin: mnR, RecallMed: mdR, RecallMax: mxR,
			BuildSecsMin: mnB, BuildSecsMed: mdB, BuildSecsMax: mxB,
		})
	}
	payload := struct {
		Cores int       `json:"gomaxprocs"`
		Rows  []jsonRow `json:"rows"`
	}{Cores: cores, Rows: rows}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

// sweepGrid builds the grid: threads {1 (serial baseline), cores/2, cores} x ef
// {64, 96, 128}. Thread counts are deduped (on a 2-core box cores/2 == 1 == the
// serial baseline) and serial cells run once (deterministic).
func sweepGrid(cores, repeats int) []profileCell {
	threadSet := dedupThreads([]uint{1, uint(max(cores/2, 1)), uint(cores)})
	efSet := []uint{64, 96, 128, 192}
	var grid []profileCell
	for _, th := range threadSet {
		r := repeats
		if th <= 1 {
			r = 1
		}
		for _, ef := range efSet {
			grid = append(grid, profileCell{threads: th, ef: ef, repeats: r})
		}
	}
	return grid
}

func dedupThreads(in []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func runSweep(ctx context.Context, t *testing.T, grid []profileCell, big map[bucketKey][]nodeVec, queries map[bucketKey][]nodeVec, oracle map[bucketKey]map[edge]struct{}) []cellResult {
	var results []cellResult
	for _, c := range grid {
		for k, nvs := range big {
			res := cellResult{cell: c, repo: k.repo, branch: k.branch, nodes: len(nvs)}
			for rep := 0; rep < c.repeats; rep++ {
				use, err := vector.NewVectorStorage(vector.BackendUsearch, t.TempDir(),
					vector.WithExpansionAdd(c.ef), vector.WithBuildThreads(c.threads))
				if err != nil {
					t.Fatalf("new usearch (%s): %v", c.label(), err)
				}
				start := time.Now()
				upsertOne(ctx, t, use, k, nvs)
				res.buildSecs = append(res.buildSecs, time.Since(start).Seconds())

				useEdges, _, _ := candidates(ctx, t, use, k, queries[k])
				_, _, shared := diff(oracle[k], useEdges)
				recall := 1.0
				if n := len(oracle[k]); n > 0 {
					recall = float64(shared) / float64(n)
				}
				res.recalls = append(res.recalls, recall)

				if d, ok := use.(interface{ Destroy() }); ok {
					d.Destroy()
				}
			}
			mnR, mdR, mxR := minMedMax(res.recalls)
			mnB, mdB, mxB := minMedMax(res.buildSecs)
			t.Logf("%-16s  %5d nodes  recall[min/med/max]=%.4f/%.4f/%.4f  build s[min/med/max]=%.2f/%.2f/%.2f  (%d runs)",
				c.label(), res.nodes, mnR, mdR, mxR, mnB, mdB, mxB, len(res.recalls))
			results = append(results, res)
		}
	}
	return results
}

// upsertOne loads a single bucket's vectors into a store as one batch (the
// boot-rehydrate shape: one big UpsertEmbeddings per bucket).
func upsertOne(ctx context.Context, t *testing.T, store interface {
	UpsertEmbeddings(context.Context, string, string, []domain.EmbeddingRow) error
}, k bucketKey, nvs []nodeVec) {
	batch := make([]domain.EmbeddingRow, len(nvs))
	for i, nv := range nvs {
		batch[i] = domain.EmbeddingRow{NodeID: nv.nodeID, ContentHash: nv.hash, ModelID: nv.modelID, Vector: nv.vec}
	}
	if err := store.UpsertEmbeddings(ctx, k.repo, k.branch, batch); err != nil {
		t.Fatalf("upsert %s/%s: %v", k.repo, k.branch, err)
	}
}

func minMedMax(xs []float64) (mn, md, mx float64) {
	if len(xs) == 0 {
		return 0, 0, 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[0], s[len(s)/2], s[len(s)-1]
}

func renderProfileSweep(results []cellResult, cores, minNodes, repeats int) string {
	// Group rows by bucket (largest first), since the decision is per-bucket and
	// a small bucket's swing must not be read as the large bucket's signal.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].nodes != results[j].nodes {
			return results[i].nodes > results[j].nodes
		}
		return false // preserve grid order within a bucket
	})

	var b strings.Builder
	b.WriteString("## usearch build-profile sweep (recall vs build-speed)\n\n")
	fmt.Fprintf(&b, "Host GOMAXPROCS=%d. Parallel cells repeated %d x; serial cells run once (deterministic). ",
		cores, repeats)
	fmt.Fprintf(&b, "Only buckets with >= %d nodes measured (parallel build is serial below that floor). ", minNodes)
	b.WriteString("Recall = autolink edge agreement vs the exact memvec oracle, reported PER BUCKET (never aggregated across buckets - small buckets swing far more than large ones). ")
	b.WriteString("The decision signal is the recall min/max SPREAD on parallel cells, not the median: a wide swing means nondeterministic recall - a no-ship for that preset.\n\n")
	b.WriteString("| bucket | nodes | profile | runs | recall min | recall med | recall max | build s min | build s med | build s max |\n")
	b.WriteString("|---|--:|---|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, r := range results {
		mnR, mdR, mxR := minMedMax(r.recalls)
		mnB, mdB, mxB := minMedMax(r.buildSecs)
		fmt.Fprintf(&b, "| %s@%s | %d | %s | %d | %.4f | %.4f | %.4f | %.2f | %.2f | %.2f |\n",
			shortRepo(r.repo), r.branch, r.nodes, r.cell.label(), len(r.recalls), mnR, mdR, mxR, mnB, mdB, mxB)
	}
	b.WriteString("\nReference: the serial/ef64 row per bucket is the historical shipping build. ")
	b.WriteString("A parallel preset only earns the default if it holds recall parity with serial AND its min/max spread stays tight across repeats, ON THE LARGE BUCKET (the small-bucket swing is expected and not decision-relevant).\n")
	return b.String()
}
