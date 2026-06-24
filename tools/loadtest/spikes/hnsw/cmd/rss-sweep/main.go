// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

// cmd/rss-sweep measures peak RSS and warm p95 latency for the production
// UsearchStore (float16) at 50k, 250k, 500k, and 1M vectors.
// For each population it:
//
//	Builds the index in batches of 1000
//	Measures RSS after loading (before queries)
//	Runs 200 warm Search queries (seed=42 hold-out, seed=999)
//	Measures RSS at steady-state (after warm queries)
//	Records p95 latency at k=10
//	Records build time
//
// Results are printed as a Markdown table and written to RESULTS.md
// in the same directory as the source file.
// Build:
//
//	SO=$(find $(go env GOMODCACHE) -name "libusearch_c.so" 2>/dev/null | head -1)
//	SODIR=$(dirname $SO)
//	CGO_LDFLAGS="-L${SODIR} -lusearch_c" CGO_CFLAGS="-I${SODIR}" \
//	  go build -tags hnsw_native./tools/loadtest/spikes/hnsw/cmd/rss-sweep/
//
// Run:
//
//	LD_LIBRARY_PATH=${SODIR}./rss-sweep
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
)

const (
	repoID  = "rss-bench"
	branch  = "main"
	modelID = "nomic-embed-text"
	batchSz = 5000

	nWarmQueries = 200
	corpusSeed   = 42
	holdOutSeed  = 999

	rssThresholdMiB = 1536 // 1.5 GiB
)

var populations = []int{50_000, 250_000, 500_000, 1_000_000}

type sweepResult struct {
	population   int
	buildTimeSec float64
	rssLoadMiB   int64
	rssSteadyMiB int64
	p95ms        float64
}

func main() {
	fmt.Fprintln(os.Stderr, "==> usearch RSS and p95 sweep (50k/250k/500k/1M, float16)")

	var results []sweepResult
	for _, pop := range populations {
		fmt.Fprintf(os.Stderr, "\n--- population %dk ---\n", pop/1000)
		r, err := runSweep(pop)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL @%dk: %v\n", pop/1000, err)
			os.Exit(1)
		}
		results = append(results, r)
		fmt.Fprintf(os.Stderr, "    build=%.1fs  rss_load=%dMiB  rss_steady=%dMiB  p95=%.2fms\n",
			r.buildTimeSec, r.rssLoadMiB, r.rssSteadyMiB, r.p95ms)
	}

	md := renderMarkdown(results)
	fmt.Println(md)

	outPath := resultsPath()
	if err := os.WriteFile(outPath, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write RESULTS.md: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "\nwrote %s\n", outPath)
	}

	// Check 1M RSS budget.
	for _, r := range results {
		if r.population == 1_000_000 && r.rssSteadyMiB > rssThresholdMiB {
			fmt.Fprintf(os.Stderr,
				"\nWARN: 1M steady-state RSS=%dMiB exceeds 1.5GiB threshold (%dMiB) - budget risk\n",
				r.rssSteadyMiB, rssThresholdMiB)
		}
	}
}

func runSweep(pop int) (sweepResult, error) {
	ctx := context.Background()

	store, err := vector.NewUsearchStore(vector.Options{})
	if err != nil {
		return sweepResult{}, fmt.Errorf("NewUsearchStore: %w", err)
	}
	defer store.Destroy()

	filter := domain.VectorFilter{ModelID: modelID}

	// Generate corpus (pop vectors, seed=42).
	fmt.Fprintf(os.Stderr, "  generating %d corpus vectors...\n", pop)
	corpus := gen.GenerateVectors(pop, corpusSeed)

	// Build index in batches; measure build time.
	fmt.Fprintf(os.Stderr, "  building index...\n")
	buildStart := time.Now()
	for start := 0; start < pop; start += batchSz {
		end := start + batchSz
		if end > pop {
			end = pop
		}
		batch := make([]domain.EmbeddingRow, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, domain.EmbeddingRow{
				NodeID:      fmt.Sprintf("node-%d", i),
				ContentHash: fmt.Sprintf("hash-%d", i),
				ModelID:     modelID,
				Vector:      corpus[i],
			})
		}
		if err := store.UpsertEmbeddings(ctx, repoID, branch, batch); err != nil {
			return sweepResult{}, fmt.Errorf("UpsertEmbeddings at start=%d: %w", start, err)
		}
		if (start/batchSz)%100 == 0 && start > 0 {
			fmt.Fprintf(os.Stderr, "    ... %d/%d inserted\n", start, pop)
		}
	}
	buildTimeSec := time.Since(buildStart).Seconds()

	// Release corpus before measuring RSS so we get production-realistic numbers.
	// In production the caller never holds onto the corpus after indexing.
	corpus = nil
	debug.FreeOSMemory()

	// RSS after loading, before queries.
	rssLoad := readRSSMiB()
	fmt.Fprintf(os.Stderr, "  rss after load: %dMiB\n", rssLoad)

	// Generate hold-out query vectors (seed=999, not in corpus).
	queryVecs := gen.GenerateVectors(nWarmQueries, holdOutSeed)

	// Run 200 warm queries; collect latencies.
	fmt.Fprintf(os.Stderr, "  running %d warm queries...\n", nWarmQueries)
	latencies := make([]float64, nWarmQueries)
	for i, q := range queryVecs {
		t0 := time.Now()
		_, err := store.Search(ctx, repoID, branch, q, 10, filter)
		latencies[i] = float64(time.Since(t0).Nanoseconds()) / 1e6
		if err != nil {
			return sweepResult{}, fmt.Errorf("Search query %d: %w", i, err)
		}
	}

	// RSS after steady-state queries.
	rssSteady := readRSSMiB()

	// Compute p95.
	sort.Float64s(latencies)
	p95Idx := int(math.Ceil(float64(nWarmQueries)*0.95)) - 1
	if p95Idx < 0 {
		p95Idx = 0
	}
	p95 := latencies[p95Idx]

	return sweepResult{
		population:   pop,
		buildTimeSec: buildTimeSec,
		rssLoadMiB:   rssLoad,
		rssSteadyMiB: rssSteady,
		p95ms:        p95,
	}, nil
}

// readRSSMiB reads the process's current resident set size from /proc/self/status.
// Returns 0 on error (e.g. non-Linux platforms).
func readRSSMiB() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

func renderMarkdown(rows []sweepResult) string {
	var sb strings.Builder
	sb.WriteString("# RSS and Scale Sweep: UsearchStore (float16)\n\n")
	fmt.Fprintf(&sb, "Generated: %s\n\n", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	sb.WriteString("Corpus: 768-dim synthetic vectors, seed=42 (in index).\n")
	sb.WriteString("Queries: 200 warm queries, seed=999 (hold-out, not in index). k=10.\n")
	sb.WriteString("RSS measured via `/proc/self/status` `VmRSS` on Linux.\n\n")
	sb.WriteString("## Results\n\n")
	sb.WriteString("| Population | Build Time | RSS at Load | RSS Steady-State | Warm P95 (ms) | Budget (1.5GiB) |\n")
	sb.WriteString("|-----------|-----------|------------|-----------------|--------------|----------------|\n")
	for _, r := range rows {
		budget := "✓"
		if r.population == 1_000_000 && r.rssSteadyMiB > rssThresholdMiB {
			budget = "✗ EXCEEDS"
		} else if r.population != 1_000_000 {
			budget = "n/a"
		}
		fmt.Fprintf(&sb, "| %dk | %.1fs | %dMiB | %dMiB | %.2f | %s |\n",
			r.population/1000,
			r.buildTimeSec,
			r.rssLoadMiB,
			r.rssSteadyMiB,
			r.p95ms,
			budget,
		)
	}
	sb.WriteString("\n## Notes\n\n")
	sb.WriteString("- All measurements on Linux amd64 (same host as ADR-S0014 application-layer bench).\n")
	sb.WriteString("- float16 quantization (production default).\n")
	sb.WriteString("- Budget threshold: 1.5 GiB (1536 MiB) RSS at 1M vectors steady-state.\n")
	return sb.String()
}

func resultsPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "RESULTS.md"
	}
	return filepath.Join(filepath.Dir(file), "RESULTS.md")
}
