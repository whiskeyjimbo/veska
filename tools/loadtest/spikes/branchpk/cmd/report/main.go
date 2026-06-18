// Command report reads JSON metrics from the loader, bench, and gcsweep harnesses,
// assigns the M0 §Outcomes verdict, and writes RESULTS.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/branchpk/report"
)

func main() {
	loadFile := flag.String("load", "data/load_metrics.json", "path to load_metrics.json")
	benchFile := flag.String("bench", "data/bench_metrics.json", "path to bench_metrics.json")
	gcFile := flag.String("gc", "data/gcsweep_metrics.json", "path to gcsweep_metrics.json")
	outFile := flag.String("out", "RESULTS.md", "output path for RESULTS.md")
	flag.Parse()

	loads, err := readJSONArray[report.LoadMetrics](*loadFile)
	if err != nil {
		log.Fatalf("read load metrics: %v", err)
	}
	benches, err := readJSONArray[report.BenchResult](*benchFile)
	if err != nil {
		log.Fatalf("read bench metrics: %v", err)
	}
	gcs, err := readJSONArray[report.GCSweepResult](*gcFile)
	if err != nil {
		log.Fatalf("read gc metrics: %v", err)
	}

	if len(loads) == 0 {
		log.Fatal("load metrics array is empty")
	}
	if len(benches) == 0 {
		log.Fatal("bench metrics array is empty")
	}
	if len(gcs) == 0 {
		log.Fatal("gc metrics array is empty")
	}

	// Pick the 10% overlap entry as the representative.
	// Worst-case for disk, best-case for latency - most informative for the verdict.
	load := pickByOverlap(loads, 10)
	bench := pickBenchByOverlap(benches, 10)
	gc := gcs[0]

	// Determine whether row growth is linear.
	// With 50 branches × 100k symbols the loader inserts exactly branches×symbols rows.
	// We confirm linearity if nodeRows ≈ branches × symbols (within 5% tolerance).
	expected := int64(load.Branches) * int64(load.Symbols)
	linear := load.NodeRows >= int64(float64(expected)*0.95) &&
		load.NodeRows <= int64(float64(expected)*1.05)

	inputs := report.SpikeInputs{
		Load:                  load,
		Bench:                 bench,
		GC:                    gc,
		LinearGrowthConfirmed: linear,
	}

	v := report.AssignVerdict(inputs)
	md := report.RenderMarkdown(inputs, v)

	if err := os.MkdirAll(filepath.Dir(*outFile), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(*outFile, []byte(md), 0o644); err != nil {
		log.Fatalf("write %s: %v", *outFile, err)
	}

	fmt.Printf("Verdict: %s\n", v)
	fmt.Printf("Node p95: %.2f ms  Edges p95: %.2f ms  Disk: %.2f GiB  GC wall: %d ms\n",
		bench.NodeLatency.P95Ms,
		bench.EdgesLatency.P95Ms,
		float64(load.DBBytes)/float64(1<<30),
		gc.WallMs,
	)
	fmt.Printf("RESULTS.md written to %s\n", *outFile)
}

func readJSONArray[T any](path string) ([]T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var items []T
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return items, nil
}

func pickByOverlap(items []report.LoadMetrics, pct int) report.LoadMetrics {
	for _, m := range items {
		if m.OverlapPct == pct {
			return m
		}
	}
	return items[0]
}

func pickBenchByOverlap(items []report.BenchResult, pct int) report.BenchResult {
	for _, m := range items {
		if m.OverlapPct == pct {
			return m
		}
	}
	return items[0]
}
