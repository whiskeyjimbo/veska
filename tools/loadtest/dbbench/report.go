//go:build eval

package dbbench

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// WriteReport renders RESULTS.md to path.
func WriteReport(path string, drivers []string, results []WorkloadResult, cfg RunConfig, seed SeedConfig) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return RenderReport(f, drivers, results, cfg, seed)
}

// RenderReport writes the report to w.
func RenderReport(w io.Writer, drivers []string, results []WorkloadResult, cfg RunConfig, seed SeedConfig) error {
	byWorkload := map[string][]WorkloadResult{}
	for _, r := range results {
		byWorkload[r.Workload] = append(byWorkload[r.Workload], r)
	}
	workloads := make([]string, 0, len(byWorkload))
	for k := range byWorkload {
		workloads = append(workloads, k)
	}
	sort.Strings(workloads)

	fmt.Fprintf(w, "# dbbench — SQLite driver comparison\n\n")
	fmt.Fprintf(w, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "Source issue: solov2-6e5r\n\n")

	fmt.Fprintf(w, "## Environment\n\n")
	fmt.Fprintf(w, "| Key | Value |\n|---|---|\n")
	fmt.Fprintf(w, "| Go | `%s` |\n", runtime.Version())
	fmt.Fprintf(w, "| GOOS/GOARCH | `%s/%s` |\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "| CPUs | `%d` |\n", runtime.NumCPU())
	fmt.Fprintf(w, "| Drivers | `%s` |\n", strings.Join(drivers, ", "))
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "## Workload config\n\n")
	fmt.Fprintf(w, "| Key | Value |\n|---|---|\n")
	fmt.Fprintf(w, "| Seed nodes | %d |\n", seed.Nodes)
	fmt.Fprintf(w, "| Edges per src | %d |\n", seed.EdgesPerSrc)
	fmt.Fprintf(w, "| Embedding dim | %d |\n", seed.EmbedDim)
	fmt.Fprintf(w, "| graph_read iters | %d |\n", cfg.GraphIters)
	fmt.Fprintf(w, "| fts_query iters | %d |\n", cfg.FTSIters)
	fmt.Fprintf(w, "| queue_poll iters | %d |\n", cfg.QueueIters)
	fmt.Fprintf(w, "| promotion_tx iters | %d |\n", cfg.PromotionTx)
	fmt.Fprintf(w, "| bulk_ingest iters × batch | %d × %d |\n", cfg.BulkIters, cfg.BulkBatchSize)
	fmt.Fprintf(w, "| rehydrate runs | %d |\n", cfg.RehydrateRuns)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "## Results\n\n")
	for _, wl := range workloads {
		fmt.Fprintf(w, "### %s\n\n", wl)
		fmt.Fprintf(w, "| Driver | p50 ms | p95 ms | p99 ms | max ms | ops/s |\n")
		fmt.Fprintf(w, "|---|---:|---:|---:|---:|---:|\n")
		rows := byWorkload[wl]
		sort.Slice(rows, func(i, j int) bool { return rows[i].Driver < rows[j].Driver })
		for _, r := range rows {
			fmt.Fprintf(w, "| %s | %.3f | %.3f | %.3f | %.3f | %.1f |\n",
				r.Driver, r.P50Ms, r.P95Ms, r.P99Ms, r.MaxMs, r.OpsPerS)
		}
		fmt.Fprintf(w, "\n")
	}

	fmt.Fprintf(w, "## Verdict\n\n")
	fmt.Fprintf(w, "%s\n", verdict(byWorkload, drivers))
	return nil
}

// verdict picks the fastest driver per workload (lower p50 wins) and
// recommends a swap only if one driver wins ≥4 of the 6 workloads by ≥20%
// at p50 vs current production (mattn).
func verdict(byWorkload map[string][]WorkloadResult, drivers []string) string {
	const incumbent = "mattn"
	var sb strings.Builder
	wins := map[string]int{}
	margin := map[string]float64{} // total relative speedup vs incumbent

	wls := make([]string, 0, len(byWorkload))
	for k := range byWorkload {
		wls = append(wls, k)
	}
	sort.Strings(wls)

	for _, wl := range wls {
		rows := byWorkload[wl]
		var inc float64
		for _, r := range rows {
			if r.Driver == incumbent {
				inc = r.P50Ms
			}
		}
		fastest := ""
		fastestMs := 1e18
		for _, r := range rows {
			if r.P50Ms > 0 && r.P50Ms < fastestMs {
				fastestMs = r.P50Ms
				fastest = r.Driver
			}
		}
		if fastest != "" {
			wins[fastest]++
		}
		if fastest != "" && fastest != incumbent && inc > 0 {
			margin[fastest] += (inc - fastestMs) / inc
		}
		fmt.Fprintf(&sb, "- **%s**: fastest = `%s` (%.3fms p50)\n", wl, fastest, fastestMs)
	}
	fmt.Fprintf(&sb, "\nWins by driver: ")
	for _, d := range drivers {
		fmt.Fprintf(&sb, "`%s`=%d ", d, wins[d])
	}
	fmt.Fprintf(&sb, "\n\nRecommendation: hand-review the per-workload table; this harness does not auto-pick a winner. ")
	fmt.Fprintf(&sb, "If a non-incumbent driver wins ≥4 of 6 workloads with ≥20%% p50 improvement on the write paths ")
	fmt.Fprintf(&sb, "(promotion_tx, bulk_ingest), file a follow-up to swap. zombiezen would need an adapter rewrite of internal/infrastructure/sqlite/*.\n")
	return sb.String()
}
