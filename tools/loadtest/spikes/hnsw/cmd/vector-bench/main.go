// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

// cmd/vector-bench measures application-layer recall@10 and warm p95 latency for
// the production UsearchStore (VectorStorage port) at 50k and 250k vectors.
// It uses the same synthetic corpus and hold-out methodology as the raw usearch
// spike (tools/loadtest/spikes/hnsw/cmd/hnsw-eval) but exercises the
// UsearchStore adapter in internal/infrastructure/vector rather than the raw
// usearch index directly.
// Results are printed as a Markdown table and written to RESULTS.md in the
// same directory as the binary.
// Build:
//
//	LD_LIBRARY_PATH=<path-to-libusearch_c.so> \
//	  go run -tags hnsw_native./tools/loadtest/spikes/hnsw/cmd/vector-bench/
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/recall"
)

const (
	repoID  = "bench"
	branch  = "main"
	modelID = "nomic-embed-text"
	batchSz = 1000

	nHoldOut     = 100
	nWarmQueries = 200
	corpusSeed   = 42
	holdOutSeed  = 999
	warmSeed     = 7777

	nSmall = 50_000
	nLarge = 250_000

	// DoD floors
	recallFloor50k  = 0.95
	recallFloor250k = 0.85
	p95Budget50k    = 100.0 // ms
	p95Budget250k   = 150.0 // ms
)

type result struct {
	population int
	recall10   float64
	p95ms      float64
	passRecall bool
	passP95    bool
}

func main() {
	var rows []result
	for _, pop := range []int{nSmall, nLarge} {
		fmt.Fprintf(os.Stderr, "==> population %dk: building store...\n", pop/1000)
		r, err := run(pop)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL @%dk: %v\n", pop/1000, err)
			os.Exit(1)
		}
		rows = append(rows, r)
		fmt.Fprintf(os.Stderr, "    recall@10=%.4f  p95=%.2fms\n", r.recall10, r.p95ms)
	}

	md := renderMarkdown(rows)
	fmt.Println(md)

	outPath := resultsPath()
	if err := os.WriteFile(outPath, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write RESULTS.md: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "wrote %s\n", outPath)
	}

	// Exit non-zero if any DoD floor is missed.
	allPass := true
	for _, r := range rows {
		if !r.passRecall || !r.passP95 {
			allPass = false
		}
	}
	if !allPass {
		os.Exit(2)
	}
}

func run(pop int) (result, error) {
	ctx := context.Background()

	store, err := vector.NewUsearchStore()
	if err != nil {
		return result{}, fmt.Errorf("NewUsearchStore: %w", err)
	}
	defer store.Destroy()

	// Build corpus and insert in batches.
	corpus := gen.GenerateVectors(pop, corpusSeed)
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
			return result{}, fmt.Errorf("UpsertEmbeddings at start=%d: %w", start, err)
		}
	}

	filter := domain.VectorFilter{ModelID: modelID}

	// Pre-warm: run nWarmQueries that are not measured.
	warmVecs := gen.GenerateVectors(nWarmQueries, warmSeed)
	for _, q := range warmVecs {
		_, _ = store.Search(ctx, repoID, branch, q, 10, filter)
	}

	// Hold-out queries (seed=999, not in corpus).
	holdOut := gen.GenerateVectors(nHoldOut, holdOutSeed)

	// Measure recall@10 and latency in a single pass.
	latencies := make([]float64, nHoldOut)
	var sumRecall float64

	for i, q := range holdOut {
		// Ground truth via brute force (1-indexed rowids).
		gt1 := recall.BruteForceKNN(corpus, q, 10)

		t0 := time.Now()
		hits, err := store.Search(ctx, repoID, branch, q, 10, filter)
		latencies[i] = float64(time.Since(t0).Nanoseconds()) / 1e6
		if err != nil {
			return result{}, fmt.Errorf("Search: %w", err)
		}

		// Convert NodeIDs "node-{i}" to 1-based rowids for recall computation.
		ret1 := make([]int64, 0, len(hits))
		for _, h := range hits {
			var idx int
			if _, err := fmt.Sscanf(h.NodeID, "node-%d", &idx); err == nil {
				ret1 = append(ret1, int64(idx+1))
			}
		}
		sumRecall += recall.ComputeRecall(gt1, ret1)
	}

	sort.Float64s(latencies)
	p95Idx := max(int(math.Ceil(float64(nHoldOut)*0.95))-1, 0)
	p95 := latencies[p95Idx]
	r10 := sumRecall / float64(nHoldOut)

	var recallFloor, p95Budget float64
	if pop == nSmall {
		recallFloor = recallFloor50k
		p95Budget = p95Budget50k
	} else {
		recallFloor = recallFloor250k
		p95Budget = p95Budget250k
	}

	return result{
		population: pop,
		recall10:   r10,
		p95ms:      p95,
		passRecall: r10 >= recallFloor,
		passP95:    p95 <= p95Budget,
	}, nil
}

func renderMarkdown(rows []result) string {
	var sb strings.Builder
	sb.WriteString("# Application-Layer Bench: UsearchStore (VectorStorage)\n\n")
	fmt.Fprintf(&sb, "Generated: %s\n\n", time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	sb.WriteString("Exercises the production `UsearchStore` via `UpsertEmbeddings` / `Search` (not the raw spike index adapter).\n")
	sb.WriteString("Corpus: 768-dim synthetic vectors, seed=42. Hold-out: 100 queries, seed=999.\n\n")
	sb.WriteString("## Results\n\n")
	sb.WriteString("| Population | Recall@10 | P95 (ms) | Recall Floor | P95 Budget | Pass |\n")
	sb.WriteString("|-----------|-----------|----------|-------------|-----------|------|\n")
	for _, r := range rows {
		var recallFloor, p95Budget float64
		if r.population == nSmall {
			recallFloor = recallFloor50k
			p95Budget = p95Budget50k
		} else {
			recallFloor = recallFloor250k
			p95Budget = p95Budget250k
		}
		pass := "✓"
		if !r.passRecall || !r.passP95 {
			pass = "✗"
		}
		fmt.Fprintf(&sb, "| %dk | %.4f (≥%.2f) | %.2f (≤%.0f) | %.2f | %.0f | %s |\n",
			r.population/1000,
			r.recall10, recallFloor,
			r.p95ms, p95Budget,
			recallFloor, p95Budget,
			pass,
		)
	}
	sb.WriteString("\n## DoD Floors\n\n")
	sb.WriteString("- recall@10 ≥ 0.95 @50k\n")
	sb.WriteString("- recall@10 ≥ 0.85 @250k\n")
	sb.WriteString("- p95 ≤ 100ms @50k\n")
	sb.WriteString("- p95 ≤ 150ms @250k\n")
	return sb.String()
}

func resultsPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "RESULTS.md"
	}
	return filepath.Join(filepath.Dir(file), "RESULTS.md")
}
